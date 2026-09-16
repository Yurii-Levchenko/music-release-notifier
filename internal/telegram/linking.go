package telegram

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Username is the bot's own @name, which the extension API needs to build a
// deep link. Exposed on the bot rather than reaching for its client, so the
// API depends on one thing instead of two.
func (b *Bot) Username() string { return b.client.Username() }

// usernameOf reads the display name off an update, tolerating its absence.
//
// telego models From as a pointer because Telegram omits it for channel posts,
// and the username is display-only anyway (C1: a bot cannot address anyone by
// it), so a missing one is not worth failing over.
func usernameOf(from *telego.User) string {
	if from == nil {
		return ""
	}
	return from.Username
}

// LinkRedeemer binds an extension install to the user who typed the code.
type LinkRedeemer interface {
	Redeem(ctx context.Context, secret string, userID int64) (installID string, err error)
}

// WithLinking enables extension linking. Optional: without it the bot answers a
// link attempt with "not available" rather than pretending to have worked.
func (b *Bot) WithLinking(r LinkRedeemer) *Bot {
	b.linking = r
	return b
}

// tryRedeem attempts to consume a link secret and reports whether it did.
//
// false means no usable token matched — wrong, expired or already redeemed —
// and the caller decides what that means, because the two callers need
// different things. A /start payload is unambiguously a link attempt, so it
// says the code is stale. A six-character message is ambiguous: it could be an
// artist called BONES, so it falls through to searching rather than lecturing
// somebody about a code they never typed.
func (b *Bot) tryRedeem(ctx context.Context, chatID int64, username, secret string, log *slog.Logger) (handled bool) {
	if b.linking == nil {
		return false
	}

	// The user may never have pressed /start — they could have arrived with a
	// deep link, or typed the code as their first message. Upserting here is
	// idempotent and means redemption never fails for want of a row.
	userID, err := b.users.UpsertUser(ctx, chatID, username)
	if err != nil {
		log.Error("upsert user before redeem", "err", err)
		b.reply(ctx, chatID, "Щось зламалося на моєму боці. Спробуй ще раз за хвилину.", log)
		return true
	}

	install, err := b.linking.Redeem(ctx, secret, userID)
	if errors.Is(err, storage.ErrLinkNotFound) {
		return false
	}
	if err != nil {
		log.Error("redeem link token", "err", err)
		b.reply(ctx, chatID, "Не вдалося підключити Spotify. Спробуй ще раз за хвилину.", log)
		return true
	}

	// The install id is logged and the secret is not. One identifies which
	// extension linked; the other is a credential.
	log.Info("extension linked", "install_id", install, "user_id", userID)
	b.reply(ctx, chatID, linkedText(), log)
	return true
}

func linkedText() string {
	return "✅ <b>Spotify підключено</b>\n\n" +
		"Тепер можна підписуватись прямо з клієнта Spotify: правий клік на виконавцеві → " +
		"<i>Notify me about releases</i>.\n\n" +
		"Підписки спільні — те, що додано тут, видно там, і навпаки."
}

// linkExpiredText answers a code that looks right but is not usable.
//
// Said without distinguishing wrong from expired, because the person holding it
// cannot act on the difference: either way the fix is to press the button in
// the extension again. Guessing games about which codes exist are also not
// something to help with.
func linkExpiredText() string {
	return "Цей код не підходить — імовірно, він застарів (вони живуть 15 хвилин) " +
		"або вже був використаний.\n\nНатисни кнопку підключення в розширенні ще раз, " +
		"щоб отримати новий."
}
