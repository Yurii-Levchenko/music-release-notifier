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

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
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
	beat func()
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
		beat: func() {},
	}
}

// botAliveInterval is how often an idle bot reports that it is still listening.
// Comfortably inside the health budget so one missed tick is not an alert.
const botAliveInterval = time.Minute

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

	// allowed_updates is deliberately narrow. my_chat_member is what tells us a
	// user blocked the bot, which is cheaper and earlier than waiting for a 403
	// on the next send.
	updates, err := b.client.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Timeout:        30,
		AllowedUpdates: []string{"message", "callback_query", "my_chat_member"},
	})
	if err != nil {
		return fmt.Errorf("start long polling: %w", err)
	}

	b.log.Info("bot polling for updates", "bot", b.client.Username())

	return b.consume(ctx, updates)
}

// consume is the update loop, split out from Run so it can be driven by a test
// channel. Long polling is created inside telego, so without this seam the
// most important behavior here — what happens when polling dies — could only
// be verified by reading it.
func (b *Bot) consume(ctx context.Context, updates <-chan telego.Update) error {
	// A tick that only exists to prove this loop is alive and the updates
	// channel is still open. Without it an idle bot never beats — long polling
	// delivers nothing when nobody is typing — and a healthy bot would be
	// reported as wedged.
	alive := time.NewTicker(botAliveInterval)
	defer alive.Stop()

	b.beat()

	for {
		select {
		case <-ctx.Done():
			b.log.Info("bot stopping")
			return nil

		case <-alive.C:
			b.beat()

		case update, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					// Expected: telego closes the channel on shutdown.
					b.log.Info("bot stopping")
					return nil
				}
				// Long polling died on its own. Returning nil here would look
				// like a clean exit to the errgroup: the process would keep
				// running with no bot, and nobody would find out — which is
				// exactly how the 14-hour silence happened. Fail loudly so the
				// group cancels and the restart policy does its job.
				return errors.New("telegram long polling stopped unexpectedly")
			}
			b.handle(ctx, &update)
			b.beat()
		}
	}
}

// handle dispatches one update. It never returns an error: one bad update must
// not stop the loop, or a single malformed message becomes an outage.
func (b *Bot) handle(ctx context.Context, u *telego.Update) {
	switch {
	case u.Message != nil:
		b.handleMessage(ctx, u.Message)
	case u.MyChatMember != nil:
		b.handleMyChatMember(ctx, u.MyChatMember)
	case u.CallbackQuery != nil:
		b.handleCallback(ctx, u.CallbackQuery)
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
		b.handleStop(ctx, chatID, log)
	case "":
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
		// The extension linking flow lands here in S8. Acknowledge rather than
		// silently ignore, so a stale deep link is not confusing.
		log.Info("start payload received but linking is not implemented yet")
		b.reply(ctx, chatID,
			"Прив'язка розширення Spotify ще не готова — вона з'явиться пізніше.\n"+
				"Але сам бот уже працює: /help", log)
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

func helpText(botUsername string) string {
	name := botUsername
	if name == "" {
		name = "цей бот"
	} else {
		name = "@" + name
	}
	return "<b>Release Radar</b>\n\n" +
		"Я повідомляю, коли виконавець, на якого ти підписаний, випускає новий " +
		"альбом, сингл або EP.\n\n" +
		"Spotify дає тобі підписатися на виконавця, але надійно не повідомляє " +
		"про релізи. Я закриваю цю прогалину.\n\n" +
		"<b>Команди</b>\n" +
		"/search — знайти виконавця\n" +
		"/list — мої підписки\n" +
		"/stop — відписатися від усього\n\n" +
		"Дані про релізи — з MusicBrainz і ListenBrainz.\n" +
		"<i>" + html.EscapeString(name) + " ще в розробці: працює /start, решта — на підході.</i>"
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
