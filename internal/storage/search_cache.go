package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QueryHashLen is how many hex characters of the query digest identify a cached
// search. Telegram allows 64 bytes of callback_data (SPEC.md C6), so the handle
// has to be short; 12 hex characters is 48 bits, which is plenty for a cache
// that expires weekly.
const QueryHashLen = 12

// SearchCache stores artist-search results so repeated lookups do not spend a
// MusicBrainz request. MusicBrainz allows one request per second per IP
// (SPEC.md C25), and popular names get searched over and over.
//
// The payload is stored opaquely: this package does not import musicbrainz, so
// the search layer owns its own encoding.
type SearchCache struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

// NewSearchCache takes the freshness window so callers never have to remember
// it, and so a miss and a stale entry are the same thing to everyone upstream.
func NewSearchCache(pool *pgxpool.Pool, ttl time.Duration) *SearchCache {
	return &SearchCache{pool: pool, ttl: ttl}
}

// NormalizeQuery is the cache key derivation. Two users typing "Radiohead" and
// "  radiohead " must hit the same entry, or the cache is decorative.
func NormalizeQuery(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// HashQuery returns the short handle for a query, safe to put in callback_data.
func HashQuery(q string) string {
	sum := sha256.Sum256([]byte(NormalizeQuery(q)))
	return hex.EncodeToString(sum[:])[:QueryHashLen]
}

// CachedSearch is what a lookup returns.
type CachedSearch struct {
	Query     string
	Hash      string
	Payload   []byte
	FetchedAt time.Time
}

// Get returns the cached entry for a query. found is false for both "never
// searched" and "searched too long ago": upstream has nothing to do differently.
func (c *SearchCache) Get(ctx context.Context, query string) (CachedSearch, bool, error) {
	normalized := NormalizeQuery(query)
	if normalized == "" {
		return CachedSearch{}, false, nil
	}

	var out CachedSearch
	err := c.pool.QueryRow(ctx, `
		SELECT query, query_hash, payload, fetched_at
		FROM artist_search_cache
		WHERE query = $1 AND fetched_at > now() - $2::interval`,
		normalized, c.ttl.String(),
	).Scan(&out.Query, &out.Hash, &out.Payload, &out.FetchedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return CachedSearch{}, false, nil
	}
	if err != nil {
		return CachedSearch{}, false, fmt.Errorf("read search cache for %q: %w", normalized, err)
	}
	return out, true, nil
}

// GetAnyAge returns a cached entry regardless of how old it is.
//
// This is the upstream-outage fallback. A week-old list of artist names is
// vastly better than an error message: artist identities do not change, and the
// alternative is telling someone to come back later. Only call it after a live
// fetch has already failed — served results should say they are stale.
func (c *SearchCache) GetAnyAge(ctx context.Context, query string) (CachedSearch, bool, error) {
	normalized := NormalizeQuery(query)
	if normalized == "" {
		return CachedSearch{}, false, nil
	}

	var out CachedSearch
	err := c.pool.QueryRow(ctx, `
		SELECT query, query_hash, payload, fetched_at
		FROM artist_search_cache
		WHERE query = $1`,
		normalized,
	).Scan(&out.Query, &out.Hash, &out.Payload, &out.FetchedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return CachedSearch{}, false, nil
	}
	if err != nil {
		return CachedSearch{}, false, fmt.Errorf("read stale search cache for %q: %w", normalized, err)
	}
	return out, true, nil
}

// GetByHash resolves the handle carried in a button's callback_data.
//
// A hash collision would return the other query's candidates. With 48 bits and
// a weekly expiry that is not a practical concern, and the stored query text is
// returned so the caller can show what was actually searched.
func (c *SearchCache) GetByHash(ctx context.Context, hash string) (CachedSearch, bool, error) {
	// A wrong-length hash means an old or hand-edited button, not a database
	// problem. Treat it as a miss without asking Postgres.
	if len(hash) != QueryHashLen {
		return CachedSearch{}, false, nil
	}

	var out CachedSearch
	err := c.pool.QueryRow(ctx, `
		SELECT query, query_hash, payload, fetched_at
		FROM artist_search_cache
		WHERE query_hash = $1 AND fetched_at > now() - $2::interval`,
		hash, c.ttl.String(),
	).Scan(&out.Query, &out.Hash, &out.Payload, &out.FetchedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return CachedSearch{}, false, nil
	}
	if err != nil {
		return CachedSearch{}, false, fmt.Errorf("read search cache by hash %q: %w", hash, err)
	}
	return out, true, nil
}

// Put stores or refreshes an entry.
//
// Two conflicts are possible and both are handled by overwriting: the same query
// searched again (the common case, refreshes fetched_at) and a hash collision
// between two different queries (vanishingly rare, last writer wins and Get's
// exact-query check keeps the loser correct).
func (c *SearchCache) Put(ctx context.Context, query string, payload []byte) (string, error) {
	normalized := NormalizeQuery(query)
	if normalized == "" {
		return "", errors.New("storage: refusing to cache an empty query")
	}
	hash := HashQuery(normalized)

	_, err := c.pool.Exec(ctx, `
		INSERT INTO artist_search_cache (query, query_hash, payload, fetched_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (query) DO UPDATE
			SET payload = EXCLUDED.payload,
			    query_hash = EXCLUDED.query_hash,
			    fetched_at = now()`,
		normalized, hash, payload)
	if err != nil {
		return "", fmt.Errorf("write search cache for %q: %w", normalized, err)
	}
	return hash, nil
}

// HashQuery is exposed as a method so a consumer can depend on the interface
// alone and never reach for the package-level function.
func (c *SearchCache) HashQuery(query string) string { return HashQuery(query) }

// EvictStale removes expired entries and reports how many went. Called on a
// slow timer; the cache is a convenience, so this never needs to be precise.
func (c *SearchCache) EvictStale(ctx context.Context) (int64, error) {
	tag, err := c.pool.Exec(ctx,
		`DELETE FROM artist_search_cache WHERE fetched_at < now() - $1::interval`,
		c.ttl.String())
	if err != nil {
		return 0, fmt.Errorf("evict search cache: %w", err)
	}
	return tag.RowsAffected(), nil
}
