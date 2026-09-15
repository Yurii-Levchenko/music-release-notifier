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
	"time"

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

// Where a result set came from. Logged on every search so cache behavior is
// visible without turning on debug logging — a successful search used to emit
// nothing at all, which made "is the cache even working?" unanswerable from
// the logs.
const (
	sourceCache    = "cache"
	sourceUpstream = "musicbrainz"
	// sourceStale means upstream failed and an expired entry was served instead
	// of an error. It deserves its own value: it looks like success to the user
	// and must not look like success in the logs.
	sourceStale = "stale-cache"
)

// ArtistSearcher is the slice of MusicBrainz the bot needs. Declared on the
// consumer side so the bot can be tested without an HTTP client.
type ArtistSearcher interface {
	SearchArtist(ctx context.Context, query string, limit int) ([]musicbrainz.Artist, error)
	// ArtistLinks costs a second request — the search index carries no
	// relationships at all — so it is called once per artist at subscribe time
	// and never while rendering a card.
	ArtistLinks(ctx context.Context, mbid string) (musicbrainz.Links, error)
}

// SearchCache is the slice of storage the picker needs.
//
// found is a bool rather than a sentinel error on purpose: a miss and a stale
// entry are the same thing here, and neither is a failure worth an error value
// traveling across a package boundary.
type SearchCache interface {
	Get(ctx context.Context, query string) (entry storage.CachedSearch, found bool, err error)
	// GetAnyAge ignores the freshness window. Used only after a live fetch has
	// already failed, so an outage degrades to stale data instead of an error.
	GetAnyAge(ctx context.Context, query string) (entry storage.CachedSearch, found bool, err error)
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

	started := time.Now()

	// Fast path. A cache hit answers in single-digit milliseconds, so a
	// "searching" placeholder would only flicker and cost a second API call.
	if artists, hash, ok := b.fromCache(ctx, query, log); ok {
		b.logSearch(log, query, sourceCache, len(artists), started)
		b.present(ctx, chatID, 0, query, artists, hash, "", log)
		return
	}

	// Slow path. Going upstream costs a rate-limiter slot and, when MusicBrainz
	// is under load, several seconds of 503 retries. That used to be silence:
	// nothing for seven seconds, then an error. Say something first and edit it
	// in place when the answer arrives.
	placeholder := b.sendPlaceholder(ctx, chatID, log)

	artists, hash, stale, err := b.fetchOrStale(ctx, query, log)

	// A failed search must not be logged as a search that found nothing. Both
	// end with zero results, but one is an upstream outage and the other is a
	// name that does not exist — and a dashboard that counts them together
	// reports an outage as normal traffic.
	if err != nil {
		log.Error("artist search failed",
			"query", query,
			"duration", time.Since(started).Round(time.Millisecond),
			"err", err)
		b.finish(ctx, chatID, placeholder, upstreamDownText(), nil, log)
		return
	}

	source := sourceUpstream
	if stale {
		source = sourceStale
	}
	b.logSearch(log, query, source, len(artists), started)

	switch {
	case len(artists) == 0:
		b.finish(ctx, chatID, placeholder, noResultsText(query), nil, log)
	default:
		note := ""
		if stale {
			note = staleNotice
		}
		b.present(ctx, chatID, placeholder, query, artists, hash, note, log)
	}
}

// decodeCandidates parses a cached payload. Shared so the search path and the
// subscription buttons cannot drift apart on how a payload is read.
func decodeCandidates(payload []byte) ([]musicbrainz.Artist, error) {
	var out []musicbrainz.Artist
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decode cached candidates: %w", err)
	}
	return out, nil
}

// fromCache answers from a fresh cache entry, or reports that it could not.
func (b *Bot) fromCache(ctx context.Context, query string, log *slog.Logger) ([]musicbrainz.Artist, string, bool) {
	cached, found, err := b.cache.Get(ctx, query)
	if err != nil {
		// A broken cache must degrade to a slower search, never to a failed one.
		log.Warn("search cache read failed, querying upstream", "err", err)
		return nil, "", false
	}
	if !found {
		return nil, "", false
	}
	artists, err := decodeCandidates(cached.Payload)
	if err != nil {
		// A payload we cannot decode is worse than no payload; let a fresh
		// result overwrite it.
		log.Warn("discarding undecodable cache entry", "query", query)
		return nil, "", false
	}
	return artists, cached.Hash, true
}

// fetchOrStale queries MusicBrainz and, when that fails, falls back to whatever
// is cached regardless of age.
//
// stale is true when the fallback was used, so the caller can label the answer.
// An artist's identity does not change week to week, which makes a week-old list
// enormously more useful than an apology.
func (b *Bot) fetchOrStale(ctx context.Context, query string, log *slog.Logger) (
	artists []musicbrainz.Artist, hash string, stale bool, err error,
) {
	artists, err = b.search.SearchArtist(ctx, query, searchLimit)
	if err == nil {
		payload, marshalErr := json.Marshal(artists)
		if marshalErr != nil {
			return nil, "", false, fmt.Errorf("encode search payload: %w", marshalErr)
		}
		hash, putErr := b.cache.Put(ctx, query, payload)
		if putErr != nil {
			// Losing the write costs one upstream request next time. The buttons
			// still need a handle, and it is derived from the query.
			log.Warn("search cache write failed", "err", putErr)
			hash = b.cache.HashQuery(query)
		}
		return artists, hash, false, nil
	}

	// An empty query never had a chance upstream and has nothing cached either.
	if errors.Is(err, musicbrainz.ErrEmptyQuery) {
		return nil, "", false, err
	}

	cached, found, cacheErr := b.cache.GetAnyAge(ctx, query)
	if cacheErr != nil {
		log.Warn("stale cache lookup failed", "err", cacheErr)
		return nil, "", false, err
	}
	if !found {
		return nil, "", false, err
	}
	stalled, decodeErr := decodeCandidates(cached.Payload)
	if decodeErr != nil {
		return nil, "", false, err
	}

	log.Warn("upstream unavailable, serving stale results",
		"query", query, "cached_at", cached.FetchedAt, "err", err)
	return stalled, cached.Hash, true, nil
}

// sendPlaceholder posts "searching" and returns the message id to edit, or 0 if
// it could not be sent. Zero is not worth aborting for: the search still runs
// and the answer still arrives, just without the interim message.
func (b *Bot) sendPlaceholder(ctx context.Context, chatID int64, log *slog.Logger) int {
	msg, err := b.client.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: telego.ChatID{ID: chatID},
		Text:   searchingText,
	})
	if err != nil {
		if IsUserGone(err) {
			if serr := b.users.SetBlocked(ctx, chatID, true); serr != nil {
				log.Error("record block status", "err", serr)
			}
			return 0
		}
		log.Warn("send placeholder failed", "err", err)
		return 0
	}
	return msg.MessageID
}

// present shows the first candidate, editing the placeholder when there is one.
func (b *Bot) present(ctx context.Context, chatID int64, placeholder int, query string,
	artists []musicbrainz.Artist, hash, note string, log *slog.Logger,
) {
	text, markup := renderCandidate(query, artists, 0, hash,
		b.subscribed(ctx, chatID, artists[0].MBID, log), b.now())
	b.finish(ctx, chatID, placeholder, note+text, markup, log)
}

// finish delivers the final text: editing the placeholder if one was sent, and
// otherwise sending a fresh message.
func (b *Bot) finish(ctx context.Context, chatID int64, placeholder int, text string,
	markup *telego.InlineKeyboardMarkup, log *slog.Logger,
) {
	if placeholder != 0 {
		_, err := b.client.api.EditMessageText(ctx, &telego.EditMessageTextParams{
			ChatID:             telego.ChatID{ID: chatID},
			MessageID:          placeholder,
			Text:               text,
			ParseMode:          telego.ModeHTML,
			ReplyMarkup:        markup,
			LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
		})
		if err == nil || isNotModified(err) {
			return
		}
		// The placeholder may have been deleted by the user. Fall through and
		// send the answer as a new message rather than losing it.
		log.Warn("edit placeholder failed, sending a new message", "err", err)
	}

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

// logSearch emits the one line per search that makes cache behavior visible.
func (b *Bot) logSearch(log *slog.Logger, query, source string, results int, started time.Time) {
	// The same place the log line is emitted, so the metrics cannot disagree
	// with the logs about where an answer came from. `source` is one of a
	// fixed set defined in this package, so the label stays bounded.
	elapsed := time.Since(started)
	b.metrics.SearchCache.WithLabelValues(source).Inc()
	b.metrics.SearchSeconds.WithLabelValues(source).Observe(elapsed.Seconds())

	log.Info("artist search",
		"query", query,
		"source", source,
		"results", results,
		"duration", elapsed.Round(time.Millisecond))
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

	candidates, err := decodeCandidates(cached.Payload)
	if err != nil {
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

	text, markup := renderCandidate(cached.Query, candidates, index, hash,
		b.subscribed(ctx, cq.From.ID, candidates[index].MBID, log), b.now())

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
// now is threaded through rather than read inside, so the age on a card can be
// tested end to end. Reading the clock deeper down would leave the caller
// untested, and the last bug in this function was exactly that: a parameter
// the caller supplied and the body ignored.
func renderCandidate(query string, candidates []musicbrainz.Artist, index int, hash string, subscribed bool, now time.Time) (string, *telego.InlineKeyboardMarkup) {
	a := candidates[index]

	var b strings.Builder
	fmt.Fprintf(&b, "🔎 Результати для <b>%s</b>\n\n", html.EscapeString(query))
	fmt.Fprintf(&b, "<b>%s</b>\n", html.EscapeString(a.Name))

	if facts := artistFacts(a, now); facts != "" {
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
	if subscribed {
		b.WriteString("\n\n🔔 <b>Ти підписаний на релізи цього виконавця.</b>")
	}

	return b.String(), candidateKeyboard(hash, index, len(candidates), subscribed)
}

// artistFacts is the one-line summary: what kind of act, from where, and when.
//
// now is a parameter because an age depends on today's date, and a fact that
// changes with the calendar has to be testable without waiting for a birthday.
func artistFacts(a musicbrainz.Artist, now time.Time) string {
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

	if span := lifeSpan(a, now); span != "" {
		parts = append(parts, span)
	}

	return strings.Join(parts, " · ")
}

// lifeSpan renders MusicBrainz's life-span according to what the artist is.
//
// The same field means two different things: for a Group it is the date the
// act formed, and for a Person it is a birth date. Labeling both "з" produced
// "Drake · CA · з 1986-10-24", which reads as a career start and is his
// birthday — he released his first mixtape in 2006.
//
// Career start is not available here. The artist index carries no such field
// (verified: relationships are absent from search results entirely), and
// deriving it from the earliest release would cost one query per candidate on
// an API limited to one request a second.
//
// For a living person we show the age rather than the year, because that is
// the form a reader actually parses — MusicBrainz itself prints
// "Born: 1986-10-24 (39 years ago)". It loses nothing for the card's real job
// of telling two same-named artists apart: given today's date, an age and a
// birth year carry exactly the same information.
//
// For a group the formation year stays, because for a band the year *is* the
// informative fact — it places them in an era, and "40 years old" does not.
func lifeSpan(a musicbrainz.Artist, now time.Time) string {
	begin, end := year(a.Begin), year(a.End)

	switch {
	case begin != "" && end != "":
		// A closed range needs no label: for a group it reads as the active
		// period, for a person as a lifetime, and both are correct. An age
		// would be wrong here — the dead do not have a current one.
		return begin + "–" + end

	case begin != "" && a.Type == "Person":
		if age, ok := ageAt(a.Begin, now); ok {
			return ageWords(age)
		}
		// The date was too coarse to place a birthday, so fall back to the
		// year rather than printing an age that could be off by one while
		// looking authoritative about it.
		return "нар. " + begin

	case begin != "":
		// Group, Orchestra, Choir — and an unknown type, where "formed" is
		// the safer reading: an unlabeled year is less wrong than a birth date
		// asserted about something that may not be a person.
		return "з " + begin

	case end != "":
		return "до " + end

	default:
		return ""
	}
}

// ageAt computes an age from a MusicBrainz date, and reports whether it could
// be computed exactly.
//
// Exactness matters: from a bare year the answer is off by one for most of the
// calendar, and a card that says "39 років" when the artist is 38 is worse
// than one that says "нар. 1986" and lets the reader do the arithmetic.
func ageAt(date string, now time.Time) (int, bool) {
	born, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0, false
	}

	age := now.Year() - born.Year()
	if now.YearDay() < born.YearDay() {
		// The birthday has not happened yet this year.
		age--
	}
	if age < 0 || age > 130 {
		// A date MusicBrainz accepted that cannot describe a living person.
		// Say nothing rather than print nonsense.
		return 0, false
	}
	return age, true
}

// ageWords applies Ukrainian plural agreement, which has three forms chosen by
// the last two digits: 21 рік, 22 роки, 25 років — and 11 років, not
// "11 рік", which is the case a units-only rule gets wrong.
func ageWords(age int) string {
	tens := age % 100
	units := age % 10

	switch {
	case tens >= 11 && tens <= 14:
		return fmt.Sprintf("%d років", age)
	case units == 1:
		return fmt.Sprintf("%d рік", age)
	case units >= 2 && units <= 4:
		return fmt.Sprintf("%d роки", age)
	default:
		return fmt.Sprintf("%d років", age)
	}
}

// year keeps just the year from a MusicBrainz date, which arrives as
// YYYY-MM-DD, YYYY-MM or YYYY.
func year(date string) string {
	if len(date) < 4 {
		return ""
	}
	y := date[:4]
	for _, r := range y {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return y
}

// candidateKeyboard renders the navigation row plus the subscribe toggle.
//
// The label follows the state rather than being fixed, so the button always
// says what pressing it will do — and after pressing, the card is redrawn so
// the two never disagree.
func candidateKeyboard(hash string, index, total int, subscribed bool) *telego.InlineKeyboardMarkup {
	markup := navigationKeyboard(hash, index, total)

	action := telego.InlineKeyboardButton{
		Text:         "🔔 Підписатись",
		CallbackData: cbSubscribe + ":" + hash + ":" + strconv.Itoa(index),
	}
	if subscribed {
		action = telego.InlineKeyboardButton{
			Text:         "🔕 Відписатись",
			CallbackData: cbUnsubscribe + ":" + hash + ":" + strconv.Itoa(index),
		}
	}
	markup.InlineKeyboard = append(markup.InlineKeyboard,
		[]telego.InlineKeyboardButton{action})
	return markup
}

// subscribed reports whether this chat already follows the artist. A failure to
// answer must not block the card, so it degrades to "not subscribed" — the user
// then sees Subscribe, and pressing it is idempotent anyway.
func (b *Bot) subscribed(ctx context.Context, chatID int64, mbid string, log *slog.Logger) bool {
	ok, err := b.subs.IsSubscribed(ctx, chatID, mbid)
	if err != nil {
		log.Warn("subscription check failed", "mbid", mbid, "err", err)
		return false
	}
	return ok
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
