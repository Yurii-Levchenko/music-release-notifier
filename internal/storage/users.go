package storage

import (
	"context"
	"fmt"

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

// SetBlocked marks a chat as unreachable, or clears the mark.
//
// It does not delete subscriptions. A block is reversible — the user can unblock
// and pick up where they left off — so the row stays and the notifier skips it.
// Deletion belongs to /stop, which is an explicit choice by the user.
func (u *Users) SetBlocked(ctx context.Context, chatID int64, blocked bool) error {
	var err error
	if blocked {
		_, err = u.pool.Exec(ctx, `
			UPDATE users SET blocked_at = now()
			WHERE telegram_chat_id = $1 AND blocked_at IS NULL`, chatID)
	} else {
		_, err = u.pool.Exec(ctx, `
			UPDATE users SET blocked_at = NULL
			WHERE telegram_chat_id = $1`, chatID)
	}
	if err != nil {
		return fmt.Errorf("set blocked=%t for chat_id=%d: %w", blocked, chatID, err)
	}
	return nil
}
