package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subscriptions owns the user/artist relationship.
//
// Every method is keyed by Telegram chat id rather than by internal user id, so
// callers never have to resolve one first and cannot pass a stale id from
// before a /stop.
type Subscriptions struct {
	pool *pgxpool.Pool
}

func NewSubscriptions(pool *pgxpool.Pool) *Subscriptions { return &Subscriptions{pool: pool} }

// Subscription is one row of a user's list.
type Subscription struct {
	MBID string
	Name string
}

// ArtistRef is the minimum needed to subscribe. Deliberately not
// musicbrainz.Artist: storage does not depend on where the artist came from,
// and the extension will supply the same shape from a Spotify id in v2.
type ArtistRef struct {
	MBID string
	Name string
}

// Subscribe records a subscription and reports whether it is new.
//
// One statement, three upserts: the user (who may have pressed a button after
// /stop wiped them), the artist (whose name we refresh), and the subscription
// itself. Doing it in one CTE rather than three round trips means there is no
// window where a user exists with no subscription, and no transaction to leak.
//
// created is false when the pair already existed. That is not an error — the
// PRIMARY KEY on (user_id, artist_mbid) makes a duplicate impossible by
// construction (SPEC.md NFR-2), and pressing Subscribe twice is something
// people do.
func (s *Subscriptions) Subscribe(ctx context.Context, chatID int64, artist ArtistRef, source string) (created bool, err error) {
	if artist.MBID == "" {
		return false, errors.New("storage: refusing to subscribe to an artist with no MBID")
	}

	var userID int64
	err = s.pool.QueryRow(ctx, `
		WITH u AS (
			INSERT INTO users (telegram_chat_id)
			VALUES ($1)
			ON CONFLICT (telegram_chat_id) DO UPDATE SET blocked_at = NULL
			RETURNING id
		), a AS (
			INSERT INTO artists (mbid, name)
			VALUES ($2, $3)
			ON CONFLICT (mbid) DO UPDATE SET name = EXCLUDED.name
			RETURNING mbid
		)
		INSERT INTO subscriptions (user_id, artist_mbid, source)
		SELECT u.id, a.mbid, $4 FROM u, a
		ON CONFLICT DO NOTHING
		RETURNING user_id`,
		chatID, artist.MBID, artist.Name, source,
	).Scan(&userID)

	// No row returned means ON CONFLICT DO NOTHING fired: the pair was already
	// there. The distinction is what lets the bot say "already subscribed"
	// instead of pretending something happened.
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("subscribe chat=%d artist=%s: %w", chatID, artist.MBID, err)
	}
	return true, nil
}

// Unsubscribe removes one subscription and reports whether it existed.
func (s *Subscriptions) Unsubscribe(ctx context.Context, chatID int64, mbid string) (removed bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM subscriptions
		WHERE artist_mbid = $2
		  AND user_id = (SELECT id FROM users WHERE telegram_chat_id = $1)`,
		chatID, mbid)
	if err != nil {
		return false, fmt.Errorf("unsubscribe chat=%d artist=%s: %w", chatID, mbid, err)
	}
	return tag.RowsAffected() > 0, nil
}

// IsSubscribed answers whether to draw Subscribe or Unsubscribe on a card.
func (s *Subscriptions) IsSubscribed(ctx context.Context, chatID int64, mbid string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM subscriptions
			WHERE artist_mbid = $2
			  AND user_id = (SELECT id FROM users WHERE telegram_chat_id = $1)
		)`, chatID, mbid).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check subscription chat=%d artist=%s: %w", chatID, mbid, err)
	}
	return exists, nil
}

// List returns one page of a user's subscriptions plus the total count.
//
// Sorted by name so the order is stable between pages; sorting by created_at
// would shuffle the list every time someone subscribes mid-browse.
func (s *Subscriptions) List(ctx context.Context, chatID int64, limit, offset int) (items []Subscription, total int, err error) {
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions
		WHERE user_id = (SELECT id FROM users WHERE telegram_chat_id = $1)`,
		chatID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count subscriptions chat=%d: %w", chatID, err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.mbid, a.name
		FROM subscriptions s
		JOIN artists a ON a.mbid = s.artist_mbid
		WHERE s.user_id = (SELECT id FROM users WHERE telegram_chat_id = $1)
		ORDER BY a.name, a.mbid
		LIMIT $2 OFFSET $3`,
		chatID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list subscriptions chat=%d: %w", chatID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.MBID, &sub.Name); err != nil {
			return nil, 0, fmt.Errorf("scan subscription: %w", err)
		}
		items = append(items, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate subscriptions: %w", err)
	}
	return items, total, nil
}

// Forget deletes the user and everything hanging off them, and reports whether
// there was anything to delete.
//
// This is /stop. Every dependent table is ON DELETE CASCADE, so one statement
// removes subscriptions, channels, installs, link tokens and pending
// notifications. Telegram's terms require honoring a deletion request without
// undue delay (SPEC.md C30), and a soft delete would not be one.
func (s *Subscriptions) Forget(ctx context.Context, chatID int64) (existed bool, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE telegram_chat_id = $1`, chatID)
	if err != nil {
		return false, fmt.Errorf("forget chat=%d: %w", chatID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ArtistLinks are the external destinations shown in a notification.
type ArtistLinks struct {
	Spotify    string `json:"spotify,omitempty"`
	YouTube    string `json:"youtube,omitempty"`
	AppleMusic string `json:"apple_music,omitempty"`
	Instagram  string `json:"instagram,omitempty"`
}

// SetArtistLinks records the lookup result, including an empty one.
//
// Storing the empty case is the point: links_fetched_at is what separates "we
// have never looked" from "we looked and there is nothing", and without that
// distinction every subscribe would re-fetch the artists that have no links —
// forever, and against an API limited to one request a second.
func (s *Subscriptions) SetArtistLinks(ctx context.Context, mbid string, links ArtistLinks, version int) error {
	payload, err := json.Marshal(links)
	if err != nil {
		return fmt.Errorf("encode links for %s: %w", mbid, err)
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE artists
		SET links = $2::jsonb, links_fetched_at = now(), links_version = $3
		WHERE mbid = $1`, mbid, string(payload), version); err != nil {
		return fmt.Errorf("store links for %s: %w", mbid, err)
	}
	return nil
}

// NeedsLinks reports whether this artist has never had a link lookup.
//
// A missing artist row answers false: there is nothing to attach links to, and
// the caller has a worse problem than missing links.
func (s *Subscriptions) NeedsLinks(ctx context.Context, mbid string, version int) (bool, error) {
	var fetched *time.Time
	var stored int
	err := s.pool.QueryRow(ctx,
		`SELECT links_fetched_at, links_version FROM artists WHERE mbid = $1`, mbid).
		Scan(&fetched, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read link state for %s: %w", mbid, err)
	}
	// Never resolved, or resolved against an older set of link kinds.
	return fetched == nil || stored < version, nil
}

// ArtistsMissingLinks returns tracked artists that have never had a link
// lookup, oldest subscription first.
//
// Only artists somebody is actually subscribed to: looking up links for an
// artist nobody follows would spend a rate-limited request on a row that will
// never appear in a notification.
func (s *Subscriptions) ArtistsMissingLinks(ctx context.Context, limit, version int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.mbid
		FROM artists a
		WHERE (a.links_fetched_at IS NULL OR a.links_version < $2)
		  AND EXISTS (SELECT 1 FROM subscriptions s WHERE s.artist_mbid = a.mbid)
		ORDER BY a.created_at
		LIMIT $1`, limit, version)
	if err != nil {
		return nil, fmt.Errorf("find artists missing links: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var mbid string
		if err := rows.Scan(&mbid); err != nil {
			return nil, fmt.Errorf("scan artist mbid: %w", err)
		}
		out = append(out, mbid)
	}
	return out, rows.Err()
}
