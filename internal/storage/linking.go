package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Linking owns the extension handshake: minting a link token, redeeming it in
// the bot, and resolving an API token back to a user.
type Linking struct {
	db DB
}

func NewLinking(db DB) *Linking { return &Linking{db: db} }

// LinkTTL is how long a code stays usable.
//
// Fifteen minutes is a compromise between two failures. Too short and somebody
// who switched to their phone to open Telegram comes back to an expired code,
// which reads as the feature being broken. Too long and an unredeemed code sits
// in a chat history as a live credential.
const LinkTTL = 15 * time.Minute

// codeAlphabet deliberately omits 0/O and 1/I/L.
//
// The short code exists to be read off one screen and typed into another (D6,
// because ?start= payloads do not reliably arrive on repeat linking — C4).
// Every character that can be misread is a support conversation, and there is
// no reason to spend entropy on ambiguity: 31^6 is still 887 million over a
// fifteen-minute window.
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

const codeLength = 6

// ErrLinkNotFound means no usable token matched: wrong, already redeemed, or
// expired.
//
// One error for all three on purpose. Telling an unauthenticated caller which
// of those it was turns the endpoint into an oracle for guessing codes, and the
// person holding a code they just copied cannot act on the distinction anyway.
var ErrLinkNotFound = errors.New("storage: no usable link token")

// LinkToken is what /link/init hands back.
type LinkToken struct {
	// Token goes in the deep link. Stored only as a hash.
	Token string
	// ShortCode is the manual fallback the user can type to the bot.
	ShortCode string
	// APIToken is the bearer the extension will use. Minted now and inert until
	// somebody redeems Token, so the extension can hold it and poll /v1/me.
	APIToken  string
	ExpiresAt time.Time
}

// CreateLinkToken mints a token for one extension install.
func (l *Linking) CreateLinkToken(ctx context.Context, installID string) (LinkToken, error) {
	if strings.TrimSpace(installID) == "" {
		return LinkToken{}, errors.New("storage: install_id is required")
	}

	token, err := randomToken()
	if err != nil {
		return LinkToken{}, err
	}
	apiToken, err := randomToken()
	if err != nil {
		return LinkToken{}, err
	}
	expires := time.Now().Add(LinkTTL)

	// One live code per install. Pressing "link" again should replace the code
	// on screen, not add a second one that also works — two valid codes for one
	// install is a thing to explain rather than a feature, and it is what turns
	// an unauthenticated endpoint into unbounded row growth.
	//
	// Redeemed rows are untouched: they are the record of when this install was
	// linked, and deleting them would lose it.
	if _, err := l.db.Exec(ctx,
		`DELETE FROM link_tokens WHERE install_id = $1 AND redeemed_at IS NULL`,
		installID); err != nil {
		return LinkToken{}, fmt.Errorf("clear previous link tokens: %w", err)
	}

	// short_code is UNIQUE and six characters, so a collision with a live code
	// is possible however unlikely. Retrying on the constraint is the honest
	// way to handle it: checking first would be a race, and the database
	// already knows the answer.
	for attempt := 0; attempt < 5; attempt++ {
		code, codeErr := randomCode()
		if codeErr != nil {
			return LinkToken{}, codeErr
		}

		_, err = l.db.Exec(ctx, `
			INSERT INTO link_tokens (token_hash, short_code, install_id, expires_at, api_token_hash)
			VALUES ($1, $2, $3, $4, $5)`,
			hashToken(token), code, installID, expires, hashToken(apiToken))
		if err == nil {
			return LinkToken{
				Token:     token,
				ShortCode: code,
				APIToken:  apiToken,
				ExpiresAt: expires,
			}, nil
		}
		if !isUniqueViolation(err, "link_tokens_short_code_key") {
			return LinkToken{}, fmt.Errorf("create link token: %w", err)
		}
	}
	return LinkToken{}, fmt.Errorf("create link token: short code kept colliding: %w", err)
}

// Redeem binds a token to a user and returns the install it belongs to.
//
// secret is either the deep-link token or the short code; both are accepted
// because the user may arrive by either route and cannot be expected to know
// which one they have.
func (l *Linking) Redeem(ctx context.Context, secret string, userID int64) (installID string, err error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", ErrLinkNotFound
	}

	// One statement. The UPDATE ... WHERE redeemed_at IS NULL is what makes a
	// token single-use: two simultaneous redemptions both run, one matches zero
	// rows, and there is no window between checking and claiming. A
	// SELECT-then-UPDATE here would be the same race this project refuses
	// everywhere else.
	//
	// The insert into installs is in the same CTE so a redeemed token can never
	// exist without the install it was redeemed for.
	var install string
	err = l.db.QueryRow(ctx, `
		WITH claimed AS (
			UPDATE link_tokens
			SET redeemed_at = now(), user_id = $2
			WHERE redeemed_at IS NULL
			  AND expires_at > now()
			  AND (token_hash = $1 OR short_code = $3)
			RETURNING install_id, api_token_hash
		)
		INSERT INTO installs (install_id, user_id, api_token_hash, linked_at)
		SELECT claimed.install_id, $2, claimed.api_token_hash, now() FROM claimed
		ON CONFLICT (install_id) DO UPDATE
			SET user_id = EXCLUDED.user_id,
			    api_token_hash = EXCLUDED.api_token_hash,
			    linked_at = now(),
			    last_seen_at = NULL
		RETURNING install_id`,
		hashToken(secret), userID, strings.ToUpper(secret)).Scan(&install)

	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrLinkNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redeem link token: %w", err)
	}
	return install, nil
}

// Install is a linked extension.
type Install struct {
	InstallID        string
	UserID           int64
	TelegramChatID   int64
	TelegramUsername string
	LinkedAt         time.Time
}

// InstallByAPIToken resolves a bearer token. found is false for any token that
// does not match, which includes one minted at /link/init and never redeemed.
func (l *Linking) InstallByAPIToken(ctx context.Context, apiToken string) (Install, bool, error) {
	if strings.TrimSpace(apiToken) == "" {
		return Install{}, false, nil
	}

	var in Install
	var username *string
	err := l.db.QueryRow(ctx, `
		SELECT i.install_id, i.user_id, u.telegram_chat_id, u.telegram_username, i.linked_at
		FROM installs i
		JOIN users u ON u.id = i.user_id
		WHERE i.api_token_hash = $1`, hashToken(apiToken)).
		Scan(&in.InstallID, &in.UserID, &in.TelegramChatID, &username, &in.LinkedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return Install{}, false, nil
	}
	if err != nil {
		return Install{}, false, fmt.Errorf("resolve api token: %w", err)
	}
	if username != nil {
		in.TelegramUsername = *username
	}
	return in, true, nil
}

// TouchInstall records that a linked extension called in.
//
// Deliberately best-effort and separate from the lookup: a write on every
// authenticated read would put the API's availability at the mercy of a
// statistic nobody reads during an incident.
func (l *Linking) TouchInstall(ctx context.Context, installID string) error {
	if _, err := l.db.Exec(ctx,
		`UPDATE installs SET last_seen_at = now() WHERE install_id = $1`, installID); err != nil {
		return fmt.Errorf("touch install %s: %w", installID, err)
	}
	return nil
}

// DeleteExpiredLinkTokens removes tokens nobody redeemed.
//
// Redeemed rows stay: they are the record of when an install was linked and by
// which route, and they are small.
func (l *Linking) DeleteExpiredLinkTokens(ctx context.Context) (int64, error) {
	tag, err := l.db.Exec(ctx,
		`DELETE FROM link_tokens WHERE redeemed_at IS NULL AND expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired link tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// randomToken returns 32 bytes of crypto/rand as unpadded base64url — 43
// characters, URL-safe, so it survives a Telegram deep link without escaping.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomCode draws from codeAlphabet without modulo bias.
//
// Rejection sampling rather than % len(alphabet): 256 is not a multiple of 31,
// so the naive version would make the first few characters measurably more
// likely. It barely matters at this size, and it is one loop to not have to
// argue about it.
func randomCode() (string, error) {
	out := make([]byte, 0, codeLength)
	buf := make([]byte, codeLength*2)
	limit := byte(256 - (256 % len(codeAlphabet)))

	for len(out) < codeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate short code: %w", err)
		}
		for _, b := range buf {
			if b < limit {
				out = append(out, codeAlphabet[int(b)%len(codeAlphabet)])
				if len(out) == codeLength {
					break
				}
			}
		}
	}
	return string(out), nil
}

// hashToken is what goes in the database. SHA-256 and not a password hash on
// purpose: these are 32 bytes of uniform randomness, not something a person
// chose, so there is nothing for bcrypt's work factor to defend against and a
// slow hash on every authenticated request would only cost latency.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return sum[:]
}

// LooksLikeShortCode reports whether text could be a link code.
//
// Used to decide whether to try redeeming before treating a message as an
// artist search. Shape only — whether it is a live code is the database's
// answer, and a six-character artist name must still be searchable when it is
// not.
func LooksLikeShortCode(text string) bool {
	text = strings.ToUpper(strings.TrimSpace(text))
	if len(text) != codeLength {
		return false
	}
	for _, r := range text {
		if !strings.ContainsRune(codeAlphabet, r) {
			return false
		}
	}
	return true
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
