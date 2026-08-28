package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// searchLimit is how many candidates the picker offers. MusicBrainz ranks by
// relevance, and past five the hits stop being plausible answers to a name.
const searchLimit = 5

// Callback data prefixes. Telegram caps callback_data at 64 bytes (SPEC.md C6),
// so a button carries a 12-hex handle to the cached result set plus an index —
// "nav:abcdef123456:4" is 18 bytes — and never the query text itself.
const (
	cbNavigate = "nav"
	cbNoop     = "noop"
)

// ArtistSearcher is the slice of MusicBrainz the bot needs. Declared on the
// consumer side so the bot can be tested without an HTTP client.
type ArtistSearcher interface {
	SearchArtist(ctx context.Context, query string, limit int) ([]musicbrainz.Artist, error)
}

// SearchCache is the slice of storage the picker needs.
//
// found is a bool rather than a sentinel error on purpose: a miss and a stale
// entry are the same thing here, and neither is a failure worth an error value
// traveling across a package boundary.
type SearchCache interface {
	Get(ctx context.Context, query string) (entry storage.CachedSearch, found bool, err error)
	GetByHash(ctx context.Context, hash string) (entry storage.CachedSearch, found bool, err error)
	Put(ctx context.Context, query string, payload []byte) (hash string, err error)
	HashQuery(query string) string
}

// handleSearch answers a name with a browsable card of candidates.
func (b *Bot) handleSearch(ctx context.Context, chatID int64, query string, log *slog.Logger) {
	query = strings.TrimSpace(query)
	if query == "" {
		b.reply(ctx, chatID,
			"Напиши назву виконавця — наприклад <code>radiohead</code> — або <code>/search radiohead</code>.", log)
		return
	}
	// Telegram messages can be 4096 characters; an artist name cannot.
	if len([]rune(query)) > 120 {
		b.reply(ctx, chatID, "Занадто довгий запит. Спробуй лише назву виконавця.", log)
		return
	}

	candidates, hash, err := b.lookup(ctx, query, log)
	if err != nil {
		if errors.Is(err, musicbrainz.ErrEmptyQuery) {
			b.reply(ctx, chatID, "У цьому запиті немає чого шукати.", log)
			return
		}
		log.Error("artist search failed", "query", query, "err", err)
		b.reply(ctx, chatID,
			"MusicBrainz зараз не відповідає. Спробуй ще раз за хвилину.", log)
		return
	}

	if len(candidates) == 0 {
		b.reply(ctx, chatID, fmt.Sprintf(
			"Не знайшов нікого за запитом <b>%s</b>.\n\n"+
				"Спробуй іншу назву — або перевір написання на musicbrainz.org.",
			html.EscapeString(query)), log)
		return
	}

	text, markup := renderCandidate(query, candidates, 0, hash)
	if _, err := b.client.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:      telego.ChatID{ID: chatID},
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: markup,
		LinkPreviewOptions: &telego.LinkPreviewOptions{
			IsDisabled: true,
		},
	}); err != nil {
		if IsUserGone(err) {
			if serr := b.users.SetBlocked(ctx, chatID, true); serr != nil {
				log.Error("record block status", "err", serr)
			}
			return
		}
		log.Warn("send picker failed", "err", err)
	}
}

// lookup returns candidates for a query, from cache when possible.
func (b *Bot) lookup(ctx context.Context, query string, log *slog.Logger) ([]musicbrainz.Artist, string, error) {
	cached, found, err := b.cache.Get(ctx, query)
	switch {
	case err != nil:
		// A broken cache must degrade to a slower search, never to a failed one.
		log.Warn("search cache read failed, querying upstream", "err", err)
	case found:
		var artists []musicbrainz.Artist
		if jsonErr := json.Unmarshal(cached.Payload, &artists); jsonErr == nil {
			log.Debug("search cache hit", "query", query, "cached_at", cached.FetchedAt)
			return artists, cached.Hash, nil
		}
		// A payload we cannot decode is worse than no payload; fall through so
		// the fresh result overwrites it.
		log.Warn("discarding undecodable cache entry", "query", query)
	}

	artists, err := b.search.SearchArtist(ctx, query, searchLimit)
	if err != nil {
		return nil, "", err
	}

	payload, err := json.Marshal(artists)
	if err != nil {
		return nil, "", fmt.Errorf("encode search payload: %w", err)
	}
	hash, err := b.cache.Put(ctx, query, payload)
	if err != nil {
		// Losing the cache write costs one upstream request next time. The
		// buttons still need a handle, and the hash is derived from the query,
		// so compute it anyway rather than dropping the card.
		log.Warn("search cache write failed", "err", err)
		hash = b.cache.HashQuery(query)
	}
	return artists, hash, nil
}

// handleNavigate moves between candidates in an existing card.
func (b *Bot) handleNavigate(ctx context.Context, cq *telego.CallbackQuery, hash string, index int, log *slog.Logger) {
	cached, found, err := b.cache.GetByHash(ctx, hash)
	if err != nil {
		log.Warn("search cache lookup by hash failed", "hash", hash, "err", err)
	}
	if !found {
		// The entry expired, so the buttons on this old message are dead. Say so
		// instead of leaving the user tapping a card that cannot move.
		b.answerCallback(ctx, cq.ID, "Ці результати вже застаріли — пошукай ще раз.", log)
		return
	}

	var candidates []musicbrainz.Artist
	if err := json.Unmarshal(cached.Payload, &candidates); err != nil {
		b.answerCallback(ctx, cq.ID, "Не можу прочитати ці результати — пошукай ще раз.", log)
		return
	}
	if len(candidates) == 0 {
		b.answerCallback(ctx, cq.ID, "Тут нічого немає.", log)
		return
	}

	// Clamp rather than trust: callback_data comes from the client and an old
	// message may point past a shorter refreshed result set.
	if index < 0 {
		index = 0
	}
	if index >= len(candidates) {
		index = len(candidates) - 1
	}

	text, markup := renderCandidate(cached.Query, candidates, index, hash)

	if cq.Message == nil {
		b.answerCallback(ctx, cq.ID, "", log)
		return
	}
	msg := cq.Message.Message()
	if msg == nil {
		b.answerCallback(ctx, cq.ID, "", log)
		return
	}

	_, err = b.client.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      telego.ChatID{ID: msg.Chat.ID},
		MessageID:   msg.MessageID,
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: markup,
		LinkPreviewOptions: &telego.LinkPreviewOptions{
			IsDisabled: true,
		},
	})
	if err != nil {
		// "message is not modified" happens when a user double-taps the same
		// arrow. It is not worth surfacing.
		if !strings.Contains(strings.ToLower(err.Error()), "not modified") {
			log.Warn("edit picker failed", "err", err)
		}
	}
	b.answerCallback(ctx, cq.ID, "", log)
}

// answerCallback must be called for every callback query, otherwise the client
// keeps a spinner on the button until it times out (SPEC.md C8).
func (b *Bot) answerCallback(ctx context.Context, id, text string, log *slog.Logger) {
	params := &telego.AnswerCallbackQueryParams{CallbackQueryID: id}
	if text != "" {
		params.Text = text
	}
	if err := b.client.api.AnswerCallbackQuery(ctx, params); err != nil {
		log.Debug("answer callback query", "err", err)
	}
}

// renderCandidate builds the card for one candidate plus its navigation row.
//
// Text and not a photo: Cover Art Archive covers releases, not artists, so
// MusicBrainz has no artist image to show (SPEC.md C32). What it does return —
// type, country, active years, disambiguation comment and top tags —
// distinguishes two same-named artists better than a photo would.
func renderCandidate(query string, candidates []musicbrainz.Artist, index int, hash string) (string, *telego.InlineKeyboardMarkup) {
	a := candidates[index]

	var b strings.Builder
	fmt.Fprintf(&b, "🔎 Результати для <b>%s</b>\n\n", html.EscapeString(query))
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(a.Name))

	if facts := artistFacts(a); facts != "" {
		fmt.Fprintf(&b, "%s\n", html.EscapeString(facts))
	}
	if a.Disambiguation != "" {
		fmt.Fprintf(&b, "<i>%s</i>\n", html.EscapeString(a.Disambiguation))
	}
	if len(a.Tags) > 0 {
		fmt.Fprintf(&b, "%s\n", html.EscapeString(strings.Join(a.Tags, " · ")))
	}
	fmt.Fprintf(&b, "\n<a href=\"https://musicbrainz.org/artist/%s\">MusicBrainz</a>",
		html.EscapeString(a.MBID))
	b.WriteString("\n\n<i>Підписка з'явиться на наступному етапі.</i>")

	return b.String(), navigationKeyboard(hash, index, len(candidates))
}

// artistFacts is the one-line summary: what kind of act, from where, and when.
func artistFacts(a musicbrainz.Artist) string {
	parts := make([]string, 0, 3)

	switch a.Type {
	case "Group":
		parts = append(parts, "Гурт")
	case "Person":
		parts = append(parts, "Виконавець")
	case "":
		// MusicBrainz often has no type for obscure artists; say nothing rather
		// than guess.
	default:
		parts = append(parts, a.Type)
	}

	if a.Country != "" {
		parts = append(parts, a.Country)
	}

	switch {
	case a.Begin != "" && a.End != "":
		parts = append(parts, a.Begin+"–"+a.End)
	case a.Begin != "":
		parts = append(parts, "з "+a.Begin)
	case a.End != "":
		parts = append(parts, "до "+a.End)
	}

	return strings.Join(parts, " · ")
}

// navigationKeyboard renders "◀ 2/5 ▶", omitting arrows that would go nowhere.
func navigationKeyboard(hash string, index, total int) *telego.InlineKeyboardMarkup {
	row := make([]telego.InlineKeyboardButton, 0, 3)

	if index > 0 {
		row = append(row, telego.InlineKeyboardButton{
			Text:         "◀",
			CallbackData: navData(hash, index-1),
		})
	}
	row = append(row, telego.InlineKeyboardButton{
		Text:         fmt.Sprintf("%d / %d", index+1, total),
		CallbackData: cbNoop,
	})
	if index < total-1 {
		row = append(row, telego.InlineKeyboardButton{
			Text:         "▶",
			CallbackData: navData(hash, index+1),
		})
	}

	return &telego.InlineKeyboardMarkup{
		InlineKeyboard: [][]telego.InlineKeyboardButton{row},
	}
}

func navData(hash string, index int) string {
	return cbNavigate + ":" + hash + ":" + strconv.Itoa(index)
}

// parseCallbackData splits "prefix:hash:index". Returns ok=false for anything
// unexpected — callback_data arrives from the client and an old message can
// carry a format this build no longer speaks.
func parseCallbackData(data string) (prefix, hash string, index int, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	// Reject empty components here rather than passing a blank hash down to the
	// cache. The user-visible result would be the same either way, but garbage
	// that travels is garbage that shows up in logs far from its origin.
	if parts[0] == "" || parts[1] == "" {
		return "", "", 0, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", "", 0, false
	}
	return parts[0], parts[1], idx, true
}
