// Package telegram is the only package allowed to know about the Telegram Bot
// API. It exposes a notify.Notifier for outbound release messages and a bot
// worker for inbound updates; everything Telegram-shaped stops here.
package telegram

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

// Client is a thin wrapper over the Bot API. It exists so the notifier and the
// bot worker can share one connection and one error-classification path.
type Client struct {
	api     *telego.Bot
	log     *slog.Logger
	metrics *metrics.Metrics
	// self is filled by Init and used for logging and deep links.
	self *telego.User
}

func NewClient(token string, log *slog.Logger) (*Client, error) {
	// WithDiscardLogger: telego's default logger may print sensitive data, and
	// we already have structured logging.
	api, err := telego.NewBot(token, telego.WithDiscardLogger())
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &Client{api: api, log: log, metrics: metrics.Nop()}, nil
}

// Init verifies the token and caches the bot's own identity. Called once at
// startup so a bad token fails fast instead of on the first send.
func (c *Client) Init(ctx context.Context) error {
	me, err := c.api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe (is TELEGRAM_BOT_TOKEN valid?): %w", err)
	}
	c.self = me
	c.log.Info("telegram connected", "bot", me.Username, "id", me.ID)
	return nil
}

// Username returns the bot's @name without the @, or "" before Init.
func (c *Client) Username() string {
	if c.self == nil {
		return ""
	}
	return c.self.Username
}

// SendHTML sends a plain text message.
//
// parse_mode is HTML, never MarkdownV2: MarkdownV2 requires escaping 18
// characters under three context-dependent rules, and would break on ordinary
// artist names like "Panic! At The Disco". HTML needs three characters and the
// standard library already escapes them correctly.
func (c *Client) SendHTML(ctx context.Context, chatID int64, text string) error {
	_, err := c.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID:    telego.ChatID{ID: chatID},
		Text:      text,
		ParseMode: telego.ModeHTML,
		LinkPreviewOptions: &telego.LinkPreviewOptions{
			IsDisabled: true,
		},
	})
	return c.observe(err)
}

// SendPhotoHTML sends a photo by URL with an HTML caption. Telegram fetches the
// image itself, so we never download it.
func (c *Client) SendPhotoHTML(ctx context.Context, chatID int64, photoURL, caption string) error {
	_, err := c.api.SendPhoto(ctx, &telego.SendPhotoParams{
		ChatID:    telego.ChatID{ID: chatID},
		Photo:     telego.InputFile{URL: photoURL},
		Caption:   caption,
		ParseMode: telego.ModeHTML,
	})
	return c.observe(err)
}

// SetCommands registers the command list so it shows up in the client's menu.
func (c *Client) SetCommands(ctx context.Context) error {
	err := c.api.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "start", Description: "Що це і як користуватися"},
			{Command: "search", Description: "Знайти виконавця за назвою"},
			{Command: "list", Description: "Мої підписки"},
			{Command: "stop", Description: "Відписатися від усього і видалити дані"},
		},
	})
	return c.observe(err)
}

// Notifier adapts Client to notify.Notifier. Kept separate so the domain
// depends on the interface and never on this package.
type Notifier struct {
	client *Client
}

func NewNotifier(c *Client) *Notifier { return &Notifier{client: c} }

// Kind matches channels.kind in the database.
func (n *Notifier) Kind() string { return "telegram" }

// Send delivers one release to one chat. Address is the numeric chat id as a
// string: a bot cannot address an @username (SPEC C1), and telego's own docs
// say the same.
func (n *Notifier) Send(ctx context.Context, to notify.Recipient, rel notify.Release) error {
	chatID, err := parseChatID(to.Address)
	if err != nil {
		// A malformed address is our data problem, not a delivery failure.
		return &notify.DeliveryError{Disposition: notify.BadMessage, Err: err}
	}

	body := formatRelease(rel)

	// Roughly a quarter of releases have no cover art (SPEC C27b), so the
	// text-only path is normal operation, not a rare fallback.
	if rel.CoverURL == "" {
		return n.client.SendHTML(ctx, chatID, body)
	}

	return n.sendWithCover(ctx, chatID, rel, body)
}

// coverFetchRetryDelay is how long to wait before asking Telegram to fetch the
// cover a second time.
//
// Telegram downloads the image itself (D14: we never fetch it), and Cover Art
// Archive answers with a 307 to archive.org, which is occasionally slow enough
// that Telegram gives up. Observed live on 09.09.2026: the URL that failed
// served a 24 KB JPEG in 2.1 s when checked minutes later.
const coverFetchRetryDelay = 3 * time.Second

// sendWithCover sends the photo, and degrades to text only once that is
// actually hopeless.
//
// The distinction that matters: "Telegram could not download our URL" and "our
// URL is malformed" are both 400s and both classified BadMessage, but only the
// first is worth another try. Treating them the same silently downgraded a
// release to a text-only message because archive.org was slow for two seconds.
func (n *Notifier) sendWithCover(ctx context.Context, chatID int64, rel notify.Release, body string) error {
	err := n.client.SendPhotoHTML(ctx, chatID, rel.CoverURL, body)
	if err == nil {
		n.client.metrics.CoverSends.WithLabelValues("ok").Inc()
		return nil
	}

	var de *notify.DeliveryError
	if ok := asDeliveryError(err, &de); !ok || de.Disposition != notify.BadMessage {
		// Not a caption or image problem at all — a rate limit, an outage, a
		// dead chat. That belongs to the outbox, which will retry the whole
		// notification rather than quietly dropping the cover.
		return err
	}

	if isCoverFetchFailure(de.Err) {
		n.client.log.Warn("telegram could not fetch the cover, retrying once",
			"release_id", rel.ID, "err", de.Err)

		if sleepErr := sleepCtx(ctx, coverFetchRetryDelay); sleepErr != nil {
			return sleepErr
		}

		if retryErr := n.client.SendPhotoHTML(ctx, chatID, rel.CoverURL, body); retryErr == nil {
			n.client.metrics.CoverSends.WithLabelValues("retried_ok").Inc()
			return nil
		}
		n.client.log.Warn("cover still unavailable on the second try, sending text",
			"release_id", rel.ID)
	}

	// A malformed URL, or a fetch that failed twice. The notification must not
	// be lost over a missing picture.
	n.client.metrics.CoverSends.WithLabelValues("degraded_to_text").Inc()
	return n.client.SendHTML(ctx, chatID, body)
}

// isCoverFetchFailure reports whether Telegram failed to *download* the image,
// as opposed to rejecting the URL itself. Only the former can succeed on a
// retry; retrying a malformed URL just delays the text message.
func isCoverFetchFailure(err error) bool {
	if err == nil {
		return false
	}
	desc := strings.ToLower(err.Error())
	return contains(desc,
		"failed to get http url content",
		"webpage_curl_failed",
		"image_process_failed",
		"wrong type of the web page content")
}

// sleepCtx waits, or returns early if the context is canceled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// formatRelease builds the message body. Every value that came from an external
// API is escaped — artist and album titles routinely contain &, < and >.
// maxTitleRunes bounds the release title in a notification.
//
// A photo caption is limited to 1024 characters and a message to 4096, and
// MusicBrainz titles are not bounded at all — box sets and classical works run
// long. Exceeding the caption limit is survivable (the photo path degrades to
// text) but it would cost the artwork over something as avoidable as a title,
// and exceeding the message limit would cost the notification.
//
// 180 runes is well inside both and already longer than anyone reads in a chat.
const maxTitleRunes = 180

func formatRelease(rel notify.Release) string {
	var b strings.Builder
	b.WriteString("🎵 <b>")
	b.WriteString(html.EscapeString(rel.ArtistName))
	b.WriteString("</b> — новий реліз")

	// The Instagram handle goes in the header rather than with the streaming
	// links, because it answers a different question: those are "where do I
	// play this", this is "who is this".
	if handle := musicbrainz.InstagramHandle(rel.Links.Instagram); handle != "" {
		b.WriteString(" ")
		b.WriteString(link(rel.Links.Instagram, "@"+handle))
	}

	b.WriteString("\n\n")

	if rel.InfoURL != "" {
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(rel.InfoURL))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(trimRunes(rel.Title, maxTitleRunes)))
		b.WriteString("</a>")
	} else {
		b.WriteString("<b>")
		b.WriteString(html.EscapeString(trimRunes(rel.Title, maxTitleRunes)))
		b.WriteString("</b>")
	}

	b.WriteString("\n")
	b.WriteString(html.EscapeString(releaseTypeLabel(rel.PrimaryType)))
	if !rel.ReleaseDate.IsZero() {
		b.WriteString(" · ")
		b.WriteString(rel.ReleaseDate.Format("02.01.2006"))
	}

	if listen := listenRow(rel.Links); listen != "" {
		b.WriteString("\n\n")
		b.WriteString(listen)
	}
	return b.String()
}

// listenRow is the "go press play" line.
//
// It exists because the only link in a notification used to be MusicBrainz,
// which is a metadata site. Somebody who has just been told their artist
// released an album wants to listen, not to read a database entry.
//
// The URLs point at each service's own web player and were read from
// MusicBrainz, which is CC0. Nothing here comes from Spotify's API, so the
// restriction that ruled Spotify out as a data source — ToS §III.9, forwarding
// Spotify content to another service — does not apply to a hyperlink.
func listenRow(l notify.ArtistLinks) string {
	if !l.Listenable() {
		// Normal, not exceptional: an artist subscribed to before link lookups
		// existed has none, and some artists genuinely have none.
		return ""
	}

	var parts []string
	// Spotify first. This project exists because Spotify does not reliably
	// tell you about releases, so that is where most readers will want to go.
	if l.Spotify != "" {
		parts = append(parts, link(l.Spotify, "Spotify"))
	}
	if l.YouTube != "" {
		parts = append(parts, link(l.YouTube, "YouTube"))
	}
	if l.AppleMusic != "" {
		parts = append(parts, link(l.AppleMusic, "Apple Music"))
	}
	return "▶️ " + strings.Join(parts, " · ")
}

func link(href, label string) string {
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
}

func releaseTypeLabel(t string) string {
	switch t {
	case "Album":
		return "Альбом"
	case "Single":
		return "Сингл"
	case "EP":
		return "EP"
	default:
		return t
	}
}

// trimRunes shortens s to at most n runes, counting runes rather than bytes
// because Telegram's limits are in characters and half of this project's data
// is Japanese and Cyrillic — a byte-based cut would both truncate too early
// and be able to split a character in half.
func trimRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
