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

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

// Client is a thin wrapper over the Bot API. It exists so the notifier and the
// bot worker can share one connection and one error-classification path.
type Client struct {
	api *telego.Bot
	log *slog.Logger
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
	return &Client{api: api, log: log}, nil
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
	return wrap(err)
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
	return wrap(err)
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
	return wrap(err)
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

	err = n.client.SendPhotoHTML(ctx, chatID, rel.CoverURL, body)
	if err == nil {
		return nil
	}
	// A bad or unreachable image URL must not cost the user the notification.
	// Fall back to text once, then let the real disposition stand.
	var de *notify.DeliveryError
	if ok := asDeliveryError(err, &de); ok && de.Disposition == notify.BadMessage {
		n.client.log.Warn("photo send failed, falling back to text",
			"release_id", rel.ID, "err", de.Err)
		return n.client.SendHTML(ctx, chatID, body)
	}
	return err
}

// formatRelease builds the message body. Every value that came from an external
// API is escaped — artist and album titles routinely contain &, < and >.
func formatRelease(rel notify.Release) string {
	var b strings.Builder
	b.WriteString("🎵 <b>")
	b.WriteString(html.EscapeString(rel.ArtistName))
	b.WriteString("</b> — новий реліз\n\n")

	if rel.InfoURL != "" {
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(rel.InfoURL))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(rel.Title))
		b.WriteString("</a>")
	} else {
		b.WriteString("<b>")
		b.WriteString(html.EscapeString(rel.Title))
		b.WriteString("</b>")
	}

	b.WriteString("\n")
	b.WriteString(html.EscapeString(releaseTypeLabel(rel.PrimaryType)))
	if !rel.ReleaseDate.IsZero() {
		b.WriteString(" · ")
		b.WriteString(rel.ReleaseDate.Format("02.01.2006"))
	}
	return b.String()
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
