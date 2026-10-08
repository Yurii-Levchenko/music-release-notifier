package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// UserStore is the slice of storage the bot actually needs. Declared here, as a
// consumer-side interface, so the bot stays testable and never grows a
// dependency on pgx.
type UserStore interface {
	// UpsertUser records the chat and returns the internal user id. Must be
	// idempotent: /start is pressed more than once.
	UpsertUser(ctx context.Context, chatID int64, username string) (int64, error)
	// SetBlocked flips the blocked flag when the user blocks or unblocks the bot.
	SetBlocked(ctx context.Context, chatID int64, blocked bool) error
}

// Bot consumes inbound updates. Long polling, not webhooks: Telegram's own
// advice is "you use getUpdates and it works, keep it that way", and it needs
// no domain, no TLS and no inbound port (SPEC D12).
type Bot struct {
	client *Client
	users  UserStore
	search ArtistSearcher
	cache  SearchCache
	subs   SubscriptionStore
	log    *slog.Logger

	// beat reports that the bot is still working, for the health registry.
	// A no-op by default so the bot runs unmonitored in tests.
	beat    func()
	metrics *metrics.Metrics

	// now is injectable because the artist card shows an age, and a fact that
	// changes with the calendar must be testable without waiting a year.
	now func() time.Time

	// catchUp is optional; nil disables the recent-release lookup on subscribe.
	catchUp CatchUp

	// linking is optional; nil means extension linking is unavailable, and the
	// bot says so rather than accepting a code it cannot redeem.
	linking LinkRedeemer
}

// CatchUp queues anything an artist released in the last few days for a
// subscriber who just signed up. Implemented by the poller, which already owns
// the feed and the release-recording path.
type CatchUp interface {
	CatchUp(ctx context.Context, chatID int64, artistMBID string) (queued int, err error)
}

// WithCatchUp enables the recent-release lookup on subscribe.
func (b *Bot) WithCatchUp(c CatchUp) *Bot {
	b.catchUp = c
	return b
}

func NewBot(
	c *Client,
	users UserStore,
	search ArtistSearcher,
	cache SearchCache,
	subs SubscriptionStore,
	log *slog.Logger,
) *Bot {
	return &Bot{
		client: c, users: users, search: search, cache: cache, subs: subs, log: log,
		beat: func() {}, metrics: metrics.Nop(), now: time.Now,
	}
}

const (
	// pollTimeout is how long Telegram holds a getUpdates open when there is
	// nothing to deliver. It is also the beat rhythm of an idle bot: one
	// successful poll at least every 30 s, well inside the health budget.
	pollTimeout = 30 * time.Second

	// pollDeadline bounds one getUpdates end to end. telego's fasthttp caller
	// honors a context deadline and nothing else, so without one a connection
	// that stops answering would hold the loop for good.
	pollDeadline = pollTimeout + 15*time.Second

	// A failed poll is retried after pollRetryMin, doubling up to pollRetryMax.
	// The cap keeps recovery prompt once Telegram is back; the doubling keeps a
	// long outage from becoming a log line every second.
	pollRetryMin = time.Second
	pollRetryMax = 30 * time.Second
)

// allowedUpdates is deliberately narrow. my_chat_member is what tells us a user
// blocked the bot, which is cheaper and earlier than waiting for a 403 on the
// next send.
var allowedUpdates = []string{"message", "callback_query", "my_chat_member"}

// getUpdatesFunc is telego's GetUpdates, injectable so the loop's failure
// handling can be tested without Telegram.
type getUpdatesFunc func(ctx context.Context, params *telego.GetUpdatesParams) ([]telego.Update, error)

// WithMetrics attaches collectors. Separate from the constructor because the
// bot already takes six dependencies, and metrics are not one of them in the
// sense that matters — the bot works identically without them.
func (b *Bot) WithMetrics(m *metrics.Metrics) *Bot {
	if m != nil {
		b.metrics = m
		b.client.metrics = m
	}
	return b
}

// ReportProgressTo registers a callback the bot invokes whenever it proves it
// is still working. Passing a plain func keeps this package independent of the
// health registry.
func (b *Bot) ReportProgressTo(beat func()) {
	if beat != nil {
		b.beat = beat
	}
}

// Run blocks until ctx is canceled. Returns nil on a clean shutdown.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.client.SetCommands(ctx); err != nil {
		// Not fatal: the bot works without a command menu.
		b.log.Warn("could not register command list", "err", err)
	}

	b.log.Info("bot polling for updates", "bot", b.client.Username())

	return b.poll(ctx, b.client.api.GetUpdates)
}

// poll is the update loop. Its own rather than telego's UpdatesViaLongPolling,
// because that one retries a failed getUpdates every 8 s, forever, behind a
// channel that never closes — while a ticker here reported the bot alive once
// a minute regardless. A revoked token (401), a second instance polling (409)
// or a connection that stopped answering all looked like an idle bot, and the
// heartbeat stayed green through exactly the failure it exists to catch
// (review 16.09, Resilience #3).
//
// So progress is reported only after Telegram has answered. A getUpdates that
// succeeds returns within pollTimeout even when nobody is typing, which keeps
// an idle bot beating; one that cannot reach Telegram stops beating, goes
// unhealthy past its budget and withholds the heartbeat. The loop itself keeps
// retrying rather than exiting: a restart fixes none of 401, 409 or an
// outage, and would take the notifier and the poller down with it.
func (b *Bot) poll(ctx context.Context, getUpdates getUpdatesFunc) error {
	params := &telego.GetUpdatesParams{
		Timeout:        int(pollTimeout / time.Second),
		AllowedUpdates: allowedUpdates,
	}
	retry := pollRetryMin
	var wait time.Duration

	for {
		// Cancellation arrives as a channel receive, the same shape the
		// notifier and the poller use, so a clean stop is never "there was an
		// error and we returned nil".
		if wait > 0 {
			select {
			case <-ctx.Done():
				b.log.Info("bot stopping")
				return nil
			case <-time.After(wait):
			}
		}

		updates, err := b.getUpdatesOnce(ctx, getUpdates, params)
		select {
		case <-ctx.Done():
			b.log.Info("bot stopping")
			return nil
		default:
		}
		if err != nil {
			b.log.Warn("getUpdates failed, retrying", "err", err, "retry_in", retry.String())
			wait = retry
			retry = min(retry*2, pollRetryMax)
			continue
		}
		wait = 0
		retry = pollRetryMin
		b.beat()

		for i := range updates {
			// The next call's offset confirms everything before it, so an
			// update is acknowledged only once this loop has come back round —
			// a crash mid-handle gets it redelivered, as with telego.
			if updates[i].UpdateID < params.Offset {
				continue
			}
			params.Offset = updates[i].UpdateID + 1
			b.handle(ctx, &updates[i])
			b.beat()
		}
	}
}

// getUpdatesOnce runs one getUpdates under pollDeadline, and returns as soon as
// ctx is canceled. The call itself cannot be interrupted — the fasthttp caller
// checks the context only before sending — so on shutdown it is left to finish
// against its own deadline instead of holding the process for up to
// pollTimeout. Its result is discarded; the updates it may carry are not
// confirmed and come back on the next start.
func (b *Bot) getUpdatesOnce(ctx context.Context, getUpdates getUpdatesFunc, params *telego.GetUpdatesParams) ([]telego.Update, error) {
	type result struct {
		updates []telego.Update
		err     error
	}
	// A copy, because an abandoned call would otherwise share the params the
	// next one is built from.
	p := *params
	done := make(chan result, 1)
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, pollDeadline)
		defer cancel()
		updates, err := getUpdates(callCtx, &p)
		done <- result{updates, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.updates, r.err
	}
}

// handle dispatches one update. It never returns an error: one bad update must
// not stop the loop, or a single malformed message becomes an outage.
func (b *Bot) handle(ctx context.Context, u *telego.Update) {
	switch {
	case u.Message != nil:
		b.metrics.BotUpdates.WithLabelValues("message").Inc()
		b.handleMessage(ctx, u.Message)
	case u.MyChatMember != nil:
		b.metrics.BotUpdates.WithLabelValues("my_chat_member").Inc()
		b.handleMyChatMember(ctx, u.MyChatMember)
	case u.CallbackQuery != nil:
		b.metrics.BotUpdates.WithLabelValues("callback_query").Inc()
		b.handleCallback(ctx, u.CallbackQuery)
	default:
		// An update kind we did not ask for in allowed_updates. Counted rather
		// than dropped silently, because a rising "other" is the signal that
		// Telegram started sending something new.
		b.metrics.BotUpdates.WithLabelValues("other").Inc()
	}
}

// handleCallback routes a button press. Every path must answer the query, or
// the client leaves a spinner on the button until it times out (SPEC.md C8).
func (b *Bot) handleCallback(ctx context.Context, cq *telego.CallbackQuery) {
	log := b.log.With("callback_id", cq.ID)

	// The page counter in the middle of the navigation row is not actionable.
	if cq.Data == cbNoop {
		b.answerCallback(ctx, cq.ID, "", log)
		return
	}

	// Prefixes carry different payloads: a card button needs a result-set handle
	// plus an index, a list button needs only an MBID. Split once and let each
	// case state what shape it expects, rather than forcing one arity on all.
	prefix, rest, found := strings.Cut(cq.Data, ":")
	if !found || prefix == "" {
		log.Debug("unrecognized callback data", "data", cq.Data)
		b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)
		return
	}

	switch prefix {
	case cbNavigate, cbSubscribe, cbUnsubscribe:
		hash, index, ok := parseCardTarget(rest)
		if ok {
			switch prefix {
			case cbNavigate:
				b.handleNavigate(ctx, cq, hash, index, log)
			case cbSubscribe:
				b.handleSubscribe(ctx, cq, hash, index, log)
			case cbUnsubscribe:
				b.handleUnsubscribeFromCard(ctx, cq, hash, index, log)
			}
			return
		}
		// unsub also appears under /list carrying a bare MBID.
		if prefix == cbUnsubscribe && looksLikeMBID(rest) {
			b.handleUnsubscribeByMBID(ctx, cq, rest, log)
			return
		}
		log.Debug("malformed card callback", "data", cq.Data)
		b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)

	case cbList:
		page, err := strconv.Atoi(rest)
		if err != nil {
			b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)
			return
		}
		b.answerCallback(ctx, cq.ID, "", log)
		b.renderList(ctx, cq, page, log)

	case cbPick, cbPickAll, cbPickNone, cbPickApply:
		b.handleSelection(ctx, cq, prefix, rest, log)

	case cbStop:
		b.handleStopConfirm(ctx, cq, rest, log)

	default:
		log.Debug("unknown callback prefix", "prefix", prefix)
		b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)
	}
}

// parseCardTarget reads the "<hash>:<index>" tail of a search-card button.
func parseCardTarget(rest string) (hash string, index int, ok bool) {
	h, idx, found := strings.Cut(rest, ":")
	if !found || h == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(idx)
	if err != nil {
		return "", 0, false
	}
	return h, n, true
}

// looksLikeMBID is a shape check, not validation: the database is the authority
// on whether the id exists. It only has to tell an MBID apart from a card
// target so the dispatcher picks the right handler.
func looksLikeMBID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func (b *Bot) handleMessage(ctx context.Context, msg *telego.Message) {
	if msg.Chat.Type != telego.ChatTypePrivate {
		// This bot is a private-chat product. Groups have a different rate
		// limit and no meaningful subscription owner.
		return
	}

	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)
	command, args := splitCommand(text)

	log := b.log.With("chat_id", chatID, "command", command)

	switch command {
	case "/start":
		b.onStart(ctx, chatID, msg.From, args, log)
	case "/help":
		b.reply(ctx, chatID, helpText(b.client.Username()), log)
	case "/search":
		b.handleSearch(ctx, chatID, args, log)
	case "/list":
		b.handleList(ctx, chatID, log)
	case "/stop":
		b.handleStop(ctx, chatID, args, log)
	case "":
		// A link code is six characters from an unambiguous alphabet, so ruling
		// it out is free and saves a rate-limited MusicBrainz request. Shape
		// alone decides nothing: only a code matching a live token is consumed,
		// so a six-letter artist name still reaches the search.
		if storage.LooksLikeShortCode(args) &&
			b.tryRedeem(ctx, chatID, usernameOf(msg.From), args, log) {
			return
		}
		// Plain text is the common case: people type a name, not a command.
		b.handleSearch(ctx, chatID, args, log)
	default:
		b.reply(ctx, chatID,
			"Не знаю такої команди. Спробуй /help.", log)
	}
}

func (b *Bot) onStart(ctx context.Context, chatID int64, from *telego.User, payload string, log *slog.Logger) {
	username := ""
	if from != nil {
		username = from.Username
	}

	// Idempotent by construction: ON CONFLICT on telegram_chat_id.
	userID, err := b.users.UpsertUser(ctx, chatID, username)
	if err != nil {
		log.Error("upsert user", "err", err)
		b.reply(ctx, chatID, "Щось зламалося на моєму боці. Спробуй ще раз за хвилину.", log)
		return
	}
	log.Info("user started", "user_id", userID, "has_payload", payload != "")

	if payload != "" {
		// A deep link is unambiguously a link attempt, so a payload that does
		// not redeem is told so. Falling through to the welcome text would look
		// exactly like the button in the extension having done nothing.
		if b.linking == nil {
			log.Warn("start payload received but linking is not configured")
			b.reply(ctx, chatID,
				"Підключення розширення зараз недоступне. Сам бот працює: /help", log)
			return
		}
		if b.tryRedeem(ctx, chatID, username, payload, log) {
			return
		}
		log.Info("start payload did not redeem")
		b.reply(ctx, chatID, linkExpiredText(), log)
		return
	}

	b.reply(ctx, chatID, helpText(b.client.Username()), log)
}

// handleMyChatMember catches blocks and unblocks. For private chats this update
// arrives exactly when the user blocks or unblocks the bot, which is the cheap
// way to stop sending to a dead chat.
func (b *Bot) handleMyChatMember(ctx context.Context, upd *telego.ChatMemberUpdated) {
	if upd.Chat.Type != telego.ChatTypePrivate {
		return
	}
	status := upd.NewChatMember.MemberStatus()
	blocked := status == telego.MemberStatusBanned || status == telego.MemberStatusLeft

	if err := b.users.SetBlocked(ctx, upd.Chat.ID, blocked); err != nil {
		b.log.Error("record block status", "chat_id", upd.Chat.ID, "blocked", blocked, "err", err)
		return
	}
	b.log.Info("chat member status changed",
		"chat_id", upd.Chat.ID, "status", status, "blocked", blocked)
}

// reply sends a message and swallows the error after logging: an undeliverable
// reply is not worth propagating out of the update loop.
func (b *Bot) reply(ctx context.Context, chatID int64, text string, log *slog.Logger) {
	err := b.client.SendHTML(ctx, chatID, text)
	if err == nil {
		return
	}
	if IsUserGone(err) {
		log.Info("chat unreachable, marking blocked")
		if serr := b.users.SetBlocked(ctx, chatID, true); serr != nil {
			log.Error("record block status", "err", serr)
		}
		return
	}
	log.Warn("reply failed", "err", err)
}

// helpText is what /start and /help say.
//
// It has to describe what the bot does today. The line it used to end with —
// "працює /start, решта — на підході" — was written when that was true and
// stayed there through search, subscriptions, delivery and everything since,
// telling every new person that nothing worked.
//
// The two facts under "Що варто знати" are there because they are the ones
// somebody would otherwise discover as a disappointment: that a release can
// arrive a day late (the poll is daily, by design — D15), and that subscribing
// is worth doing today rather than tomorrow.
func helpText(botUsername string) string {
	return "<b>Release Radar</b>\n\n" +
		"Я повідомляю, коли виконавець, на якого ти підписаний, випускає новий " +
		"альбом, сингл або EP.\n\n" +
		"Spotify дає тобі підписатися на виконавця, але надійно не повідомляє " +
		"про релізи. Я закриваю цю прогалину.\n\n" +
		"<b>Як користуватись</b>\n" +
		"Просто напиши ім'я виконавця — покажу картку з кнопкою «Підписатись».\n\n" +
		"<b>Команди</b>\n" +
		"/search — знайти виконавця\n" +
		"/list — мої підписки: тисни номери, щоб обрати кількох, потім «Відписатись»\n" +
		"/stop — видалити всі дані\n\n" +
		"<b>Що варто знати</b>\n" +
		"• Перевіряю релізи раз на добу, тож повідомлення може прийти наступного дня " +
		"після виходу.\n" +
		"• Щойно підпишешся — одразу надішлю, якщо в артиста був реліз за останні 3 дні.\n\n" +
		"Дані — з MusicBrainz і ListenBrainz." + inDevelopmentNote(botUsername)
}

// inDevelopmentNote names what is genuinely not built yet, so the message stays
// honest in the other direction too.
func inDevelopmentNote(botUsername string) string {
	name := botUsername
	if name == "" {
		name = "Цей бот"
	} else {
		name = "@" + name
	}
	return "\n<i>" + html.EscapeString(name) +
		" ще росте: розширення для Spotify — у планах.</i>"
}

// splitCommand separates "/cmd@bot payload" into "/cmd" and "payload".
func splitCommand(text string) (command, args string) {
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	command, args, _ = strings.Cut(text, " ")
	// Telegram appends @botname when several bots share a chat.
	if at := strings.IndexByte(command, '@'); at >= 0 {
		command = command[:at]
	}
	return strings.ToLower(command), strings.TrimSpace(args)
}

func parseChatID(address string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(address), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("telegram address %q is not a numeric chat id: %w", address, err)
	}
	return id, nil
}

// asDeliveryError is errors.As with a concrete target, kept here so client.go
// does not need to import errors.
func asDeliveryError(err error, target **notify.DeliveryError) bool {
	return errors.As(err, target)
}
