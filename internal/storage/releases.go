package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Releases records detected releases and fans them out to subscribers.
type Releases struct {
	// DB rather than the pool, for the same reason Notifications takes one:
	// a pgx.Tx satisfies it, so these operations can be tested inside a
	// transaction that is rolled back instead of against live rows.
	db DB
}

func NewReleases(db DB) *Releases { return &Releases{db: db} }

// NewRelease is a release the poller wants to record.
type NewRelease struct {
	ReleaseGroupMBID string
	ArtistMBID       string
	Title            string
	PrimaryType      string // Album | Single | EP
	ReleaseDate      time.Time
	CoverURL         string
}

// FanOut is what one recorded release produced.
type FanOut struct {
	ReleaseID int64
	// Created is false when this release group was already known. The poll
	// window is deliberately wider than the poll interval (SPEC.md D15), so most
	// of what a poll sees has been seen before — that is the design working, not
	// a problem.
	Created bool
	// Notified counts notifications queued. Zero on a repeat sighting, and also
	// zero when nobody is subscribed any more.
	Notified int
}

// TrackedArtistMBIDs returns the set the poller matches the feed against.
//
// Only artists somebody is actually subscribed to: the artists table also holds
// rows created by searches that nobody followed up on, and matching against
// those would record releases nobody asked for.
func (r *Releases) TrackedArtistMBIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := r.db.Query(ctx, `SELECT DISTINCT artist_mbid FROM subscriptions`)
	if err != nil {
		return nil, fmt.Errorf("read tracked artists: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var mbid string
		if err := rows.Scan(&mbid); err != nil {
			return nil, fmt.Errorf("scan tracked artist: %w", err)
		}
		out[mbid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracked artists: %w", err)
	}
	return out, nil
}

// Record stores a release and, when it is new, queues a notification for every
// current subscriber.
//
// notify=false records without fanning out. That is the first-run seed: with a
// seven-day look-back window, the very first poll finds a week of releases that
// are new to us but not new to the user, and sending all of them would be a
// flood on day one. After the baseline exists, only genuinely new releases
// arrive.
//
// Both writes happen in one transaction. A release recorded without its
// notifications would be silently swallowed forever, because the next poll
// would see the release group as already known and fan out nothing.
func (r *Releases) Record(ctx context.Context, rel NewRelease, notify bool) (FanOut, error) {
	if rel.ReleaseGroupMBID == "" || rel.ArtistMBID == "" {
		return FanOut{}, errors.New("storage: release needs both a release group and an artist")
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return FanOut{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var out FanOut
	err = tx.QueryRow(ctx, `
		INSERT INTO releases
			(release_group_mbid, artist_mbid, title, primary_type, release_date, cover_url, dedup_key)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)
		ON CONFLICT (release_group_mbid) DO NOTHING
		RETURNING id`,
		rel.ReleaseGroupMBID, rel.ArtistMBID, rel.Title, rel.PrimaryType,
		rel.ReleaseDate, rel.CoverURL, DedupKey(rel),
	).Scan(&out.ReleaseID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Already known. Nothing to announce; the previous sighting did that.
		return FanOut{Created: false}, tx.Commit(ctx)
	case err != nil:
		return FanOut{}, fmt.Errorf("insert release %s: %w", rel.ReleaseGroupMBID, err)
	}
	out.Created = true

	if !notify {
		return out, tx.Commit(ctx)
	}

	// Fan out to whoever is subscribed right now. Someone who subscribes
	// tomorrow does not get today's release: they signed up for what comes
	// next, not for a backlog.
	//
	// blocked_at is not filtered here on purpose. A block is reversible, and the
	// notifier skips blocked users at send time — dropping the row would mean
	// the release is lost for good if they unblock.
	tag, err := tx.Exec(ctx, `
		INSERT INTO notifications (user_id, release_id)
		SELECT s.user_id, $1
		FROM subscriptions s
		WHERE s.artist_mbid = $2
		ON CONFLICT (user_id, release_id) DO NOTHING`,
		out.ReleaseID, rel.ArtistMBID)
	if err != nil {
		return FanOut{}, fmt.Errorf("fan out release %s: %w", rel.ReleaseGroupMBID, err)
	}
	out.Notified = int(tag.RowsAffected())

	return out, tx.Commit(ctx)
}

// HasAnyReleases reports whether the table has ever been populated. Used to
// decide whether a poll is the first-run seed.
func (r *Releases) HasAnyReleases(ctx context.Context) (bool, error) {
	var exists bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM releases)`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check for existing releases: %w", err)
	}
	return exists, nil
}

// DedupKey is the fallback identity for a release, used when two sources
// disagree about release groups.
//
// release_group_mbid is the primary key for deduplication (SPEC.md D7) because
// MusicBrainz release groups already merge every edition of an album. This
// composite exists as a second line of defense and to make near-duplicates
// visible in the data: normalizing the title collapses "Album (Deluxe Edition)"
// and "album deluxe edition" onto the same string.
func DedupKey(rel NewRelease) string {
	return strings.Join([]string{
		rel.ArtistMBID,
		normalizeTitle(rel.Title),
		rel.ReleaseDate.Format(time.DateOnly),
	}, "|")
}

// normalizeTitle lowercases, strips punctuation and collapses whitespace.
func normalizeTitle(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := true // suppresses a leading space

	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastWasSpace = false
		case !lastWasSpace:
			b.WriteByte(' ')
			lastWasSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// PollState records when a source was last read, so a restart does not re-poll
// immediately and a stalled poller is visible in the data.
type PollState struct {
	LastPolledAt time.Time
	LastOKAt     time.Time
	LastError    string
}

func (r *Releases) PollState(ctx context.Context, source string) (PollState, bool, error) {
	var st PollState
	var polled, ok *time.Time
	var lastErr *string

	err := r.db.QueryRow(ctx,
		`SELECT last_polled_at, last_ok_at, last_error FROM poll_state WHERE source = $1`,
		source).Scan(&polled, &ok, &lastErr)
	if errors.Is(err, pgx.ErrNoRows) {
		return PollState{}, false, nil
	}
	if err != nil {
		return PollState{}, false, fmt.Errorf("read poll state for %s: %w", source, err)
	}

	if polled != nil {
		st.LastPolledAt = *polled
	}
	if ok != nil {
		st.LastOKAt = *ok
	}
	if lastErr != nil {
		st.LastError = *lastErr
	}
	return st, true, nil
}

// RecordPoll stores the outcome of an attempt. A failure updates last_polled_at
// and last_error but leaves last_ok_at alone, so the gap between the two is how
// long the poller has been failing.
func (r *Releases) RecordPoll(ctx context.Context, source string, pollErr error) error {
	var msg *string
	if pollErr != nil {
		s := pollErr.Error()
		msg = &s
	}

	_, err := r.db.Exec(ctx, `
		INSERT INTO poll_state (source, last_polled_at, last_ok_at, last_error)
		VALUES ($1, now(), CASE WHEN $2::text IS NULL THEN now() END, $2)
		ON CONFLICT (source) DO UPDATE
			SET last_polled_at = now(),
			    last_ok_at = CASE WHEN $2::text IS NULL THEN now() ELSE poll_state.last_ok_at END,
			    last_error = $2`,
		source, msg)
	if err != nil {
		return fmt.Errorf("record poll state for %s: %w", source, err)
	}
	return nil
}

// LastSuccessfulPoll reports when the release poller last completed without an
// error. Read by the metrics collector at scrape time, so the answer survives
// restarts — an in-memory timestamp would reset on every deploy and make a
// fresh container look like a poller that has never run.
func (r *Releases) LastSuccessfulPoll(ctx context.Context) (time.Time, bool, error) {
	st, found, err := r.PollState(ctx, "listenbrainz")
	if err != nil || !found {
		return time.Time{}, false, err
	}
	if st.LastOKAt.IsZero() {
		return time.Time{}, false, nil
	}
	return st.LastOKAt, true, nil
}

// QueueCatchUp gives one subscriber a notification for a release that was
// already known, so that subscribing to an artist who released something two
// days ago is worth doing today rather than tomorrow.
//
// Only for releases that already exist. A release nobody has recorded yet is
// handled by Record, which fans out to everybody subscribed — and must, since
// the poller skips a release group it has already seen, so a release recorded
// here without a fan-out would never reach the other subscribers at all.
//
// ON CONFLICT covers the case that matters: a subscriber who already has a row
// for this release, sent or pending, does not get a second one.
func (r *Releases) QueueCatchUp(ctx context.Context, chatID int64, releaseGroupMBID string) (queued bool, err error) {
	tag, err := r.db.Exec(ctx, `
		INSERT INTO notifications (user_id, release_id, kind)
		SELECT u.id, rel.id, 'catch_up'
		FROM users u, releases rel
		WHERE u.telegram_chat_id = $1
		  AND rel.release_group_mbid = $2
		ON CONFLICT (user_id, release_id) DO NOTHING`,
		chatID, releaseGroupMBID)
	if err != nil {
		return false, fmt.Errorf("queue catch-up for chat %d, release group %s: %w",
			chatID, releaseGroupMBID, err)
	}
	return tag.RowsAffected() > 0, nil
}
