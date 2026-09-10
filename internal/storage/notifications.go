package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of *pgxpool.Pool the outbox uses.
//
// It takes an interface rather than the pool for one concrete reason: Claim is
// deliberately unscoped — it takes whatever is due, which is the whole point of
// a queue. A test running against the pool would therefore lease live rows and
// bump their attempt counters. A pgx.Tx satisfies this too, so the tests run
// inside a transaction that is rolled back and cannot reach real notifications.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Notifications is the outbox: the queue the notifier drains.
type Notifications struct {
	db DB
	// lease is how long a claimed row is hidden from other workers. If the
	// process dies mid-send, the row becomes due again after this.
	lease time.Duration
	// maxAttempts bounds retries. Past it a notification is failed rather than
	// retried forever — a message nobody can receive should stop consuming
	// rate-limit budget that live messages need.
	maxAttempts int
}

func NewNotifications(db DB, lease time.Duration, maxAttempts int) *Notifications {
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	return &Notifications{db: db, lease: lease, maxAttempts: maxAttempts}
}

// Pending is one claimed notification, with everything needed to render and
// address the message. Joined up front so sending needs no further queries.
type Pending struct {
	ID       int64
	UserID   int64
	ChatID   int64
	Attempts int

	ReleaseID   int64
	ArtistMBID  string
	ArtistName  string
	Title       string
	PrimaryType string
	ReleaseDate time.Time
	CoverURL    string
	InfoURL     string

	// Streaming links, joined from the artist. Empty when the artist was
	// subscribed to before link lookups existed.
	Spotify    string
	YouTube    string
	AppleMusic string
	Instagram  string
}

// Claim leases up to limit due notifications for this worker.
//
// Intervals are passed as seconds through make_interval rather than as a Go
// duration string cast to ::interval. The string form happens to parse for the
// values this app configures, but Postgres does not understand Go's "ns" or
// "µs" units — so it is correct only by coincidence of the current config.
//
// The lease is expressed by pushing next_attempt_at into the future rather than
// by a separate "sending" state. That means a crashed worker needs no cleanup
// job: its rows simply become due again when the lease expires. One fewer
// moving part, and one fewer state that can be left inconsistent.
//
// FOR UPDATE SKIP LOCKED makes two workers safe to run at once — each takes a
// different slice instead of blocking on the same rows. Nothing runs two
// notifiers today; the queue is built so that it could.
//
// Blocked users are excluded rather than claimed and skipped. Their rows stay
// pending and cost nothing until my_chat_member clears the flag, at which point
// the release goes out. Claiming them would burn a lease every tick to discover
// the same thing.
func (n *Notifications) Claim(ctx context.Context, limit int) ([]Pending, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := n.db.Query(ctx, `
		WITH due AS (
			SELECT n.id
			FROM notifications n
			JOIN users u ON u.id = n.user_id
			WHERE n.state = 'pending'
			  AND n.next_attempt_at <= now()
			  AND u.blocked_at IS NULL
			ORDER BY n.next_attempt_at
			LIMIT $1
			FOR UPDATE OF n SKIP LOCKED
		)
		UPDATE notifications n
		SET next_attempt_at = now() + make_interval(secs => $2),
		    attempts = n.attempts + 1
		FROM due, users u, releases r, artists a
		WHERE n.id = due.id
		  AND u.id = n.user_id
		  AND r.id = n.release_id
		  AND a.mbid = r.artist_mbid
		RETURNING n.id, n.user_id, u.telegram_chat_id, n.attempts,
		          r.id, r.artist_mbid, a.name, r.title, r.primary_type,
		          r.release_date, coalesce(r.cover_url, ''),
		          coalesce(a.links->>'spotify', ''),
		          coalesce(a.links->>'youtube', ''),
		          coalesce(a.links->>'apple_music', ''),
		          coalesce(a.links->>'instagram', '')`,
		limit, n.lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", err)
	}
	defer rows.Close()

	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.UserID, &p.ChatID, &p.Attempts,
			&p.ReleaseID, &p.ArtistMBID, &p.ArtistName, &p.Title, &p.PrimaryType,
			&p.ReleaseDate, &p.CoverURL,
			&p.Spotify, &p.YouTube, &p.AppleMusic, &p.Instagram); err != nil {
			return nil, fmt.Errorf("scan claimed notification: %w", err)
		}
		p.InfoURL = "https://musicbrainz.org/artist/" + p.ArtistMBID
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed notifications: %w", err)
	}
	return out, nil
}

// MarkSent closes a notification for good.
func (n *Notifications) MarkSent(ctx context.Context, id int64) error {
	_, err := n.db.Exec(ctx, `
		UPDATE notifications
		SET state = 'sent', sent_at = now(), last_error = NULL
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark notification %d sent: %w", id, err)
	}
	return nil
}

// Retry schedules another attempt, or gives up if the budget is spent.
//
// retryAfter overrides the backoff when the channel supplied one — Telegram's
// retry_after is not advice, and ignoring it escalates to much longer lockouts.
func (n *Notifications) Retry(ctx context.Context, id int64, attempts int, retryAfter time.Duration, reason string) (gaveUp bool, err error) {
	if attempts >= n.maxAttempts {
		if err := n.Fail(ctx, id, fmt.Sprintf("gave up after %d attempts: %s", attempts, reason)); err != nil {
			return true, err
		}
		return true, nil
	}

	delay := retryAfter
	if delay <= 0 {
		delay = backoffFor(attempts)
	}

	if _, err := n.db.Exec(ctx, `
		UPDATE notifications
		SET next_attempt_at = now() + make_interval(secs => $2), last_error = $3
		WHERE id = $1`, id, delay.Seconds(), truncateError(reason)); err != nil {
		return false, fmt.Errorf("reschedule notification %d: %w", id, err)
	}
	return false, nil
}

// Fail closes a notification without sending it, keeping the reason.
func (n *Notifications) Fail(ctx context.Context, id int64, reason string) error {
	_, err := n.db.Exec(ctx, `
		UPDATE notifications
		SET state = 'failed', last_error = $2
		WHERE id = $1`, id, truncateError(reason))
	if err != nil {
		return fmt.Errorf("mark notification %d failed: %w", id, err)
	}
	return nil
}

// DropRecipient handles a permanent delivery failure: the chat is unreachable
// for good, so the user's subscriptions go and their remaining queued
// notifications are skipped.
//
// One transaction, because a half-applied version leaves the notifier trying
// the same dead chat on every tick.
func (n *Notifications) DropRecipient(ctx context.Context, userID int64, reason string) error {
	tx, err := n.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE users SET blocked_at = now() WHERE id = $1 AND blocked_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("mark user %d blocked: %w", userID, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM subscriptions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("delete subscriptions for user %d: %w", userID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE notifications
		SET state = 'skipped', last_error = $2
		WHERE user_id = $1 AND state = 'pending'`,
		userID, truncateError(reason)); err != nil {
		return fmt.Errorf("skip queued notifications for user %d: %w", userID, err)
	}

	return tx.Commit(ctx)
}

// QueueDepth reports how much is waiting and how old the oldest item is.
//
// These are the two numbers an alert should watch (SPEC.md §15a): depth is the
// leading indicator, and the age of the oldest pending row is the actual delay
// a person is experiencing.
func (n *Notifications) QueueDepth(ctx context.Context) (pending int, oldest time.Duration, err error) {
	var oldestAt *time.Time
	if err := n.db.QueryRow(ctx, `
		SELECT count(*), min(next_attempt_at)
		FROM notifications
		WHERE state = 'pending'`).Scan(&pending, &oldestAt); err != nil {
		return 0, 0, fmt.Errorf("read queue depth: %w", err)
	}
	if oldestAt != nil {
		if age := time.Since(*oldestAt); age > 0 {
			oldest = age
		}
	}
	return pending, oldest, nil
}

// backoffFor spaces retries out: 1m, 5m, 30m, 2h, 12h.
//
// Slow on purpose. A transient failure that has already survived two retries is
// usually an outage rather than a blip, and hammering it costs rate-limit
// budget that live notifications need.
func backoffFor(attempts int) time.Duration {
	// A switch rather than an indexed table: the schedule is short, and there
	// is no index arithmetic to get wrong or to have to prove safe.
	switch {
	case attempts <= 1:
		return time.Minute
	case attempts == 2:
		return 5 * time.Minute
	case attempts == 3:
		return 30 * time.Minute
	case attempts == 4:
		return 2 * time.Hour
	default:
		return 12 * time.Hour
	}
}

// truncateError keeps last_error readable in a table dump.
func truncateError(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
