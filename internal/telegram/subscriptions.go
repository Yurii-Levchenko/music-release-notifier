package telegram

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// listPageSize is how many subscriptions fit on one screen with a tappable
// number under each. Twenty numbered buttons is three rows and still scannable.
const listPageSize = 20

// Callback prefixes for subscription actions. The unsubscribe button carries a
// full MBID rather than a handle: "unsub:" plus 36 characters is 42 bytes, well
// inside Telegram's 64-byte cap (SPEC.md C6), so there is no reason to add an
// indirection that could go stale.
const (
	cbSubscribe   = "sub"
	cbUnsubscribe = "unsub"
	cbList        = "list"
	cbStop        = "stop"
)

// SubscriptionStore is the slice of storage the subscription handlers need.
type SubscriptionStore interface {
	Subscribe(ctx context.Context, chatID int64, artist storage.ArtistRef, source string) (created bool, err error)
	Unsubscribe(ctx context.Context, chatID int64, mbid string) (removed bool, err error)
	IsSubscribed(ctx context.Context, chatID int64, mbid string) (bool, error)
	List(ctx context.Context, chatID int64, limit, offset int) (items []storage.Subscription, total int, err error)
	Forget(ctx context.Context, chatID int64) (existed bool, err error)
	NeedsLinks(ctx context.Context, mbid string, version int) (bool, error)
	SetArtistLinks(ctx context.Context, mbid string, links storage.ArtistLinks, version int) error
}

// handleSubscribe is the Subscribe button on a search card.
func (b *Bot) handleSubscribe(ctx context.Context, cq *telego.CallbackQuery, hash string, index int, log *slog.Logger) {
	candidates, query, ok := b.candidatesFor(ctx, cq, hash, log)
	if !ok {
		return
	}
	if index < 0 || index >= len(candidates) {
		b.answerCallback(ctx, cq.ID, "Цього варіанта вже немає — пошукай ще раз.", log)
		return
	}
	artist := candidates[index]

	created, err := b.subs.Subscribe(ctx, cq.From.ID,
		storage.ArtistRef{MBID: artist.MBID, Name: artist.Name}, "bot")
	if err != nil {
		log.Error("subscribe failed", "mbid", artist.MBID, "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося підписатись. Спробуй ще раз.", log)
		return
	}

	if created {
		log.Info("subscribed", "mbid", artist.MBID, "artist", artist.Name)
		b.answerCallback(ctx, cq.ID, "Підписано на "+artist.Name, log)
	} else {
		// Pressing Subscribe twice is normal. Say what is true rather than
		// pretending something happened.
		b.answerCallback(ctx, cq.ID, "Ти вже підписаний на "+artist.Name, log)
	}
	b.refreshCard(ctx, cq, query, candidates, index, hash, true, log)

	// Deliberately after the card is updated. The lookup is rate limited to
	// one request a second and shared with search, so doing it first would
	// make the button feel slow for something the user cannot see yet.
	b.ensureArtistLinks(ctx, artist.MBID, log)
}

// ensureArtistLinks fetches the artist's streaming links once, if they have
// never been looked up.
//
// Best effort by design. A failure here costs the "listen on" row in future
// notifications and nothing else, so it must not turn a successful
// subscription into an error the user sees. links_fetched_at stays null on
// failure, so the next subscribe to the same artist tries again.
func (b *Bot) ensureArtistLinks(ctx context.Context, mbid string, log *slog.Logger) {
	needed, err := b.subs.NeedsLinks(ctx, mbid, musicbrainz.LinksVersion)
	if err != nil {
		log.Warn("could not check artist links", "mbid", mbid, "err", err)
		return
	}
	if !needed {
		return
	}

	links, err := b.search.ArtistLinks(ctx, mbid)
	if err != nil {
		log.Warn("artist link lookup failed", "mbid", mbid, "err", err)
		return
	}

	if err := b.subs.SetArtistLinks(ctx, mbid, storage.ArtistLinks{
		Spotify:    links.Spotify,
		YouTube:    links.YouTube,
		AppleMusic: links.AppleMusic,
		Instagram:  links.Instagram,
	}, musicbrainz.LinksVersion); err != nil {
		log.Warn("could not store artist links", "mbid", mbid, "err", err)
		return
	}

	log.Info("artist links stored", "mbid", mbid,
		"spotify", links.Spotify != "", "youtube", links.YouTube != "",
		"apple_music", links.AppleMusic != "")
}

// handleUnsubscribeFromCard is the Unsubscribe button on a search card.
func (b *Bot) handleUnsubscribeFromCard(ctx context.Context, cq *telego.CallbackQuery, hash string, index int, log *slog.Logger) {
	candidates, query, ok := b.candidatesFor(ctx, cq, hash, log)
	if !ok {
		return
	}
	if index < 0 || index >= len(candidates) {
		b.answerCallback(ctx, cq.ID, "Цього варіанта вже немає — пошукай ще раз.", log)
		return
	}
	artist := candidates[index]

	removed, err := b.subs.Unsubscribe(ctx, cq.From.ID, artist.MBID)
	if err != nil {
		log.Error("unsubscribe failed", "mbid", artist.MBID, "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося відписатись. Спробуй ще раз.", log)
		return
	}
	if removed {
		log.Info("unsubscribed", "mbid", artist.MBID, "artist", artist.Name)
		b.answerCallback(ctx, cq.ID, "Відписано від "+artist.Name, log)
	} else {
		b.answerCallback(ctx, cq.ID, "Ти й не був підписаний на "+artist.Name, log)
	}
	b.refreshCard(ctx, cq, query, candidates, index, hash, false, log)
}

// handleUnsubscribeFromList is a numbered button under /list.
func (b *Bot) handleUnsubscribeByMBID(ctx context.Context, cq *telego.CallbackQuery, mbid string, log *slog.Logger) {
	removed, err := b.subs.Unsubscribe(ctx, cq.From.ID, mbid)
	if err != nil {
		log.Error("unsubscribe failed", "mbid", mbid, "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося відписатись. Спробуй ще раз.", log)
		return
	}
	if removed {
		log.Info("unsubscribed", "mbid", mbid)
		b.answerCallback(ctx, cq.ID, "Відписано", log)
	} else {
		b.answerCallback(ctx, cq.ID, "Цієї підписки вже немає", log)
	}
	// Redraw the list in place. Removing an item shifts everything after it, so
	// leaving the old numbering on screen would make the next tap remove the
	// wrong artist.
	b.renderList(ctx, cq, 0, log)
}

// handleListCommand answers /list.
func (b *Bot) handleList(ctx context.Context, chatID int64, log *slog.Logger) {
	items, total, err := b.subs.List(ctx, chatID, listPageSize, 0)
	if err != nil {
		log.Error("list subscriptions failed", "err", err)
		b.reply(ctx, chatID, "Не вдалося прочитати твої підписки. Спробуй ще раз.", log)
		return
	}
	if total == 0 {
		b.reply(ctx, chatID,
			"Ти ще ні на кого не підписаний.\n\n"+
				"Напиши назву виконавця — і натисни <b>Підписатись</b> на картці.", log)
		return
	}

	text, markup := renderList(items, total, 0)
	if _, err := b.client.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:             telego.ChatID{ID: chatID},
		Text:               text,
		ParseMode:          telego.ModeHTML,
		ReplyMarkup:        markup,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}); err != nil {
		b.handleSendError(ctx, chatID, err, log)
	}
}

// renderList redraws an existing list message at the given page.
func (b *Bot) renderList(ctx context.Context, cq *telego.CallbackQuery, page int, log *slog.Logger) {
	msg := callbackMessage(cq)
	if msg == nil {
		return
	}
	if page < 0 {
		page = 0
	}

	items, total, err := b.subs.List(ctx, cq.From.ID, listPageSize, page*listPageSize)
	if err != nil {
		log.Error("list subscriptions failed", "err", err)
		return
	}

	// The last item on the last page can disappear under the user, leaving the
	// page out of range. Step back rather than showing an empty screen.
	if len(items) == 0 && page > 0 {
		b.renderList(ctx, cq, page-1, log)
		return
	}

	var text string
	var markup *telego.InlineKeyboardMarkup
	if total == 0 {
		text = "Ти більше ні на кого не підписаний."
	} else {
		text, markup = renderList(items, total, page)
	}

	if _, err := b.client.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:             telego.ChatID{ID: msg.Chat.ID},
		MessageID:          msg.MessageID,
		Text:               text,
		ParseMode:          telego.ModeHTML,
		ReplyMarkup:        markup,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}); err != nil && !isNotModified(err) {
		log.Warn("edit list failed", "err", err)
	}
}

// renderList builds the list body and the grid of numbered unsubscribe buttons.
//
// Numbers rather than names on the buttons: an artist name would either be
// truncated to uselessness or make one button per row, and the number maps
// straight to the line above it.
func renderList(items []storage.Subscription, total, page int) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 <b>Твої підписки</b> — %d\n\n", total)

	offset := page * listPageSize
	for i, item := range items {
		fmt.Fprintf(&b, "%d. %s\n", offset+i+1, html.EscapeString(item.Name))
	}
	b.WriteString("\nНатисни номер, щоб відписатись.")

	pages := (total + listPageSize - 1) / listPageSize
	if pages > 1 {
		fmt.Fprintf(&b, "\nСторінка %d з %d.", page+1, pages)
	}

	var rows [][]telego.InlineKeyboardButton
	const perRow = 5
	for i, item := range items {
		if i%perRow == 0 {
			rows = append(rows, nil)
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], telego.InlineKeyboardButton{
			Text:         strconv.Itoa(offset + i + 1),
			CallbackData: cbUnsubscribe + ":" + item.MBID,
		})
	}

	if pages > 1 {
		var nav []telego.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, telego.InlineKeyboardButton{
				Text: "◀", CallbackData: cbList + ":" + strconv.Itoa(page-1),
			})
		}
		nav = append(nav, telego.InlineKeyboardButton{
			Text:         fmt.Sprintf("%d / %d", page+1, pages),
			CallbackData: cbNoop,
		})
		if page < pages-1 {
			nav = append(nav, telego.InlineKeyboardButton{
				Text: "▶", CallbackData: cbList + ":" + strconv.Itoa(page+1),
			})
		}
		rows = append(rows, nav)
	}

	return b.String(), &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// handleStop asks before deleting. /stop removes everything and cannot be
// undone, so a single mistyped character should not cost someone their list.
func (b *Bot) handleStop(ctx context.Context, chatID int64, log *slog.Logger) {
	_, total, err := b.subs.List(ctx, chatID, 1, 0)
	if err != nil {
		log.Error("list subscriptions failed", "err", err)
		b.reply(ctx, chatID, "Не вдалося прочитати твої дані. Спробуй ще раз.", log)
		return
	}
	if total == 0 {
		// Still offer deletion: a user with no subscriptions may still want
		// their row gone.
		b.reply(ctx, chatID, "У тебе немає підписок. Видаляти нічого.", log)
		return
	}

	text := fmt.Sprintf(
		"Це видалить <b>усі %d підписок</b> і всі твої дані.\n\nДію не можна скасувати.", total)
	if _, err := b.client.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		Text:      text,
		ParseMode: telego.ModeHTML,
		ReplyMarkup: &telego.InlineKeyboardMarkup{
			InlineKeyboard: [][]telego.InlineKeyboardButton{{
				{Text: "Видалити все", CallbackData: cbStop + ":yes"},
				{Text: "Скасувати", CallbackData: cbStop + ":no"},
			}},
		},
	}); err != nil {
		b.handleSendError(ctx, chatID, err, log)
	}
}

func (b *Bot) handleStopConfirm(ctx context.Context, cq *telego.CallbackQuery, answer string, log *slog.Logger) {
	msg := callbackMessage(cq)

	if answer != "yes" {
		b.answerCallback(ctx, cq.ID, "Скасовано", log)
		if msg != nil {
			b.editText(ctx, msg, "Скасовано. Нічого не видалено.", log)
		}
		return
	}

	existed, err := b.subs.Forget(ctx, cq.From.ID)
	if err != nil {
		log.Error("forget user failed", "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося видалити. Спробуй ще раз.", log)
		return
	}
	log.Info("user data deleted", "existed", existed)
	b.answerCallback(ctx, cq.ID, "Видалено", log)
	if msg != nil {
		b.editText(ctx, msg,
			"Усі твої дані видалені.\n\nЯкщо захочеш повернутись — просто напиши /start.", log)
	}
}

// candidatesFor resolves the cached candidate list behind a card's buttons.
func (b *Bot) candidatesFor(ctx context.Context, cq *telego.CallbackQuery, hash string, log *slog.Logger) ([]musicbrainz.Artist, string, bool) {
	cached, found, err := b.cache.GetByHash(ctx, hash)
	if err != nil {
		log.Warn("search cache lookup by hash failed", "hash", hash, "err", err)
	}
	if !found {
		b.answerCallback(ctx, cq.ID, "Ці результати вже застаріли — пошукай ще раз.", log)
		return nil, "", false
	}
	candidates, err := decodeCandidates(cached.Payload)
	if err != nil {
		b.answerCallback(ctx, cq.ID, "Не можу прочитати ці результати — пошукай ще раз.", log)
		return nil, "", false
	}
	return candidates, cached.Query, true
}

// refreshCard redraws a search card so the button reflects the new state.
func (b *Bot) refreshCard(ctx context.Context, cq *telego.CallbackQuery, query string,
	candidates []musicbrainz.Artist, index int, hash string, subscribed bool, log *slog.Logger,
) {
	msg := callbackMessage(cq)
	if msg == nil {
		return
	}
	text, markup := renderCandidate(query, candidates, index, hash, subscribed, b.now())
	if _, err := b.client.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:             telego.ChatID{ID: msg.Chat.ID},
		MessageID:          msg.MessageID,
		Text:               text,
		ParseMode:          telego.ModeHTML,
		ReplyMarkup:        markup,
		LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
	}); err != nil && !isNotModified(err) {
		log.Warn("refresh card failed", "err", err)
	}
}

func (b *Bot) editText(ctx context.Context, msg *telego.Message, text string, log *slog.Logger) {
	if _, err := b.client.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:    telego.ChatID{ID: msg.Chat.ID},
		MessageID: msg.MessageID,
		Text:      text,
		ParseMode: telego.ModeHTML,
	}); err != nil && !isNotModified(err) {
		log.Warn("edit message failed", "err", err)
	}
}

// callbackMessage unwraps the message a button belongs to. It is absent for
// very old messages, which Telegram no longer lets us edit.
func callbackMessage(cq *telego.CallbackQuery) *telego.Message {
	if cq.Message == nil {
		return nil
	}
	return cq.Message.Message()
}

// isNotModified matches the error Telegram returns when an edit would change
// nothing — a double tap on the same button. Not worth logging.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not modified")
}

func (b *Bot) handleSendError(ctx context.Context, chatID int64, err error, log *slog.Logger) {
	if IsUserGone(err) {
		if serr := b.users.SetBlocked(ctx, chatID, true); serr != nil {
			log.Error("record block status", "err", serr)
		}
		return
	}
	log.Warn("send failed", "err", err)
}
