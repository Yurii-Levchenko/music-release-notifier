package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Users implements telegram.UserStore.
type Users struct {
	pool *pgxpool.Pool
}

func NewUsers(pool *pgxpool.Pool) *Users { return &Users{pool: pool} }

// UpsertUser records a chat and returns its internal id.
//
// Idempotent by constraint, not by checking first: /start gets pressed
// repeatedly, and a read-then-write would race with itself. The DO UPDATE also
// clears blocked_at, because a user who just messaged us plainly has not
// blocked us — that is the unblock path.
func (u *Users) UpsertUser(ctx context.Context, chatID int64, username string) (int64, error) {
	var name *string
	if username != "" {
		name = &username
	}

	var id int64
	err := u.pool.QueryRow(ctx, `
		INSERT INTO users (telegram_chat_id, telegram_username)
		VALUES ($1, $2)
		ON CONFLICT (telegram_chat_id) DO UPDATE
			SET telegram_username = COALESCE(EXCLUDED.telegram_username, users.telegram_username),
			    blocked_at        = NULL
		RETURNING id`,
		chatID, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert user chat_id=%d: %w", chatID, err)
	}
	return id, nil
}

// SetBlocked records that a user blocked the bot, or unblocked it.
//
// Blocking means the user has left (SPEC D16, decided 18.10.2026): their
// subscriptions are deleted and anything still queued for them is skipped, in
// one transaction. This used to keep both, on the theory that a block is
// reversible — which made the outcome depend on how we found out. Learned from
// my_chat_member, the subscriptions stayed and the queued rows sat pending
// forever (inflating the queue-age alert); learned from a 403 on send, the
// notifier deleted them. One event, one outcome now, and it is the one the
// Telegram ToS asks for: data goes when keeping it stops being necessary (C30).
//
// Unblocking only clears the flag. The person starts over, as a new user
// would; there is nothing left to resume.
func (u *Users) SetBlocked(ctx context.Context, chatID int64, blocked bool) error {
	if !blocked {
		if _, err := u.pool.Exec(ctx, `
			UPDATE users SET blocked_at = NULL
			WHERE telegram_chat_id = $1`, chatID); err != nil {
			return fmt.Errorf("unblock chat_id=%d: %w", chatID, err)
		}
		return nil
	}

	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int64
	err = tx.QueryRow(ctx, `
		UPDATE users SET blocked_at = coalesce(blocked_at, now())
		WHERE telegram_chat_id = $1
		RETURNING id`, chatID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A chat we never recorded: nothing to drop.
		return nil
	}
	if err != nil {
		return fmt.Errorf("mark chat_id=%d blocked: %w", chatID, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete subscriptions for chat_id=%d: %w", chatID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notifications
		SET state = 'skipped', last_error = 'user blocked the bot'
		WHERE user_id = $1 AND state = 'pending'`, userID); err != nil {
		return fmt.Errorf("skip queued notifications for chat_id=%d: %w", chatID, err)
	}
	return tx.Commit(ctx)
}
