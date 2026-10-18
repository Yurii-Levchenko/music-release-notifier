package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Two people typing the same name with different spacing or case must hit one
// cache entry, otherwise the cache exists but never helps.
func TestNormalizeQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Radiohead", "radiohead"},
		{"  radiohead  ", "radiohead"},
		{"RADIOHEAD", "radiohead"},
		{"radio   head", "radio head"},
		{"radio\thead\n", "radio head"},
		{"Океан Ельзи", "океан ельзи"},
		{"", ""},
		{"   \t\n ", ""},
	}
	for _, tc := range cases {
		if got := storage.NormalizeQuery(tc.in); got != tc.want {
			t.Fatalf("NormalizeQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHashQuery(t *testing.T) {
	// Length is a hard requirement: it is what keeps callback_data inside
	// Telegram's 64-byte cap (SPEC.md C6).
	h := storage.HashQuery("radiohead")
	if len(h) != storage.QueryHashLen {
		t.Fatalf("hash %q has length %d, want %d", h, len(h), storage.QueryHashLen)
	}

	// Equal after normalization means equal hash, or buttons break when a user
	// retypes the same name slightly differently.
	if storage.HashQuery("Radiohead") != storage.HashQuery("  radiohead ") {
		t.Fatal("normalization is not applied before hashing")
	}
	if storage.HashQuery("radiohead") == storage.HashQuery("radiohead 2") {
		t.Fatal("different queries produced the same hash")
	}

	// Stable across builds and processes: a hash embedded in a button yesterday
	// must still resolve today. Comparing two calls in one process would prove
	// nothing, so pin the actual value.
	for q, want := range map[string]string{
		"radiohead":   "3f3371c759cd",
		"Океан Ельзи": "d7e3f4b05160",
	} {
		if got := storage.HashQuery(q); got != want {
			t.Fatalf("HashQuery(%q) = %q, want %q — changing this breaks every "+
				"button in every message already sent", q, got, want)
		}
	}
}

// assertSameJSON compares payloads by value. Postgres stores them as jsonb,
// which normalizes whitespace and key order, so bytes will not match even when
// nothing was lost.
func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("stored payload is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("expected payload is not valid JSON: %v", err)
	}
	gs, _ := json.Marshal(g)
	ws, _ := json.Marshal(w)
	if !bytes.Equal(gs, ws) {
		t.Fatalf("payload = %s, want %s", gs, ws)
	}
}

func TestSearchCacheRoundTrip(t *testing.T) {
	ctx, _ := testTx(t)
	// Needs its own pool because SearchCache takes a pool, not a transaction.
	// Cleaned up explicitly at the end.
	if testDBPool == nil {
		t.Skip("no database")
	}
	cache := storage.NewSearchCache(testDBPool, time.Hour)

	const query = "  RadioHead Test Fixture  "
	payload, err := json.Marshal([]string{"one", "two"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Cleanup(func() {
		_, _ = testDBPool.Exec(ctx,
			`DELETE FROM artist_search_cache WHERE query = $1`, storage.NormalizeQuery(query))
	})

	// Miss before anything is stored.
	if _, found, err := cache.Get(ctx, query); err != nil || found {
		t.Fatalf("Get before Put: found=%v err=%v, want found=false", found, err)
	}

	hash, err := cache.Put(ctx, query, payload)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if hash != storage.HashQuery(query) {
		t.Fatalf("Put returned hash %q, want %q", hash, storage.HashQuery(query))
	}

	// Hit by query, with the key normalized.
	entry, found, err := cache.Get(ctx, "radiohead test fixture")
	if err != nil || !found {
		t.Fatalf("Get after Put: found=%v err=%v", found, err)
	}
	// The column is jsonb, which reparses and reformats: ["one","two"] comes
	// back as ["one", "two"]. Compare the decoded value, not the bytes.
	assertSameJSON(t, entry.Payload, payload)
	if entry.Query != storage.NormalizeQuery(query) {
		t.Fatalf("stored query = %q, want normalized form", entry.Query)
	}

	// Hit by hash: this is the path a button press takes.
	byHash, found, err := cache.GetByHash(ctx, hash)
	if err != nil || !found {
		t.Fatalf("GetByHash: found=%v err=%v", found, err)
	}
	if byHash.Query != entry.Query {
		t.Fatalf("GetByHash query = %q, want %q", byHash.Query, entry.Query)
	}

	// Put again: must refresh rather than fail on the unique key.
	if _, err := cache.Put(ctx, query, []byte(`["three"]`)); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	entry, found, err = cache.Get(ctx, query)
	if err != nil || !found {
		t.Fatalf("Get after refresh: found=%v err=%v", found, err)
	}
	assertSameJSON(t, entry.Payload, []byte(`["three"]`))
}

// A stale entry and a missing entry are the same thing upstream. If expiry did
// not work, the bot would serve week-old results forever.
func TestSearchCacheExpiry(t *testing.T) {
	ctx, _ := testTx(t)
	if testDBPool == nil {
		t.Skip("no database")
	}

	const query = "expiry test fixture"
	t.Cleanup(func() {
		_, _ = testDBPool.Exec(ctx, `DELETE FROM artist_search_cache WHERE query = $1`, query)
	})

	// A 30-year freshness window: long enough that no real entry is ever stale
	// to this instance, short enough that the 50-year-old fixture is.
	const staleWindow = 30 * 365 * 24 * time.Hour
	writer := storage.NewSearchCache(testDBPool, staleWindow)
	if _, err := writer.Put(ctx, query, []byte(`["x"]`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Backdate the row rather than sleeping. Decades, not hours, and the
	// reason matters: EvictStale issues an unscoped DELETE over the whole
	// table. Backdating by hours and evicting with an hour-long TTL would
	// delete every genuine cache entry older than an hour in whatever database
	// this test happens to point at — which is exactly what happened once
	// against the development database, and looked like "the cache is broken".
	backdate(ctx, t, query, "50 years")

	if _, found, err := writer.Get(ctx, query); err != nil || found {
		t.Fatalf("stale entry was returned: found=%v err=%v", found, err)
	}
	hash := storage.HashQuery(query)
	if _, found, err := writer.GetByHash(ctx, hash); err != nil || found {
		t.Fatalf("stale entry returned by hash: found=%v err=%v", found, err)
	}

	// The outage fallback sees the same row, proving it was expiry and not
	// deletion that hid it.
	if _, found, err := writer.GetAnyAge(ctx, query); err != nil || !found {
		t.Fatalf("row is gone, not merely stale: found=%v err=%v", found, err)
	}
}

// Eviction must spare what the outage fallback still needs and remove the
// rest. A row past its TTL but inside the eviction horizon is exactly what
// GetAnyAge serves while MusicBrainz is down; deleting it at the TTL would
// leave the fallback almost empty.
func TestSearchCacheEvictionKeepsTheOutageFallback(t *testing.T) {
	ctx, _ := testTx(t)
	if testDBPool == nil {
		t.Skip("no database")
	}

	const (
		fallback = "eviction fixture: past ttl, inside horizon"
		ancient  = "eviction fixture: past horizon"
	)
	t.Cleanup(func() {
		_, _ = testDBPool.Exec(ctx,
			`DELETE FROM artist_search_cache WHERE query IN ($1, $2)`, fallback, ancient)
	})

	// TTL 30 years, so the horizon is 8 × 30 = 240. The fallback row is 50
	// years old — stale, kept — and the ancient one 300 — gone. Only rows of
	// these ages can match, whatever else is in the table (see TestSearchCacheExpiry).
	cache := storage.NewSearchCache(testDBPool, 30*365*24*time.Hour)
	for _, q := range []string{fallback, ancient} {
		if _, err := cache.Put(ctx, q, []byte(`["x"]`)); err != nil {
			t.Fatalf("Put %q: %v", q, err)
		}
	}
	backdate(ctx, t, fallback, "50 years")
	backdate(ctx, t, ancient, "300 years")

	var before int
	if err := testDBPool.QueryRow(ctx,
		`SELECT count(*) FROM artist_search_cache`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	removed, err := cache.EvictStale(ctx)
	if err != nil {
		t.Fatalf("EvictStale: %v", err)
	}
	if removed != 1 {
		t.Fatalf("EvictStale removed %d rows, want exactly 1 (the ancient fixture)", removed)
	}
	// An unscoped DELETE: assert the blast radius rather than trusting it.
	var after int
	if err := testDBPool.QueryRow(ctx,
		`SELECT count(*) FROM artist_search_cache`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if before-after != 1 {
		t.Fatalf("table went from %d to %d rows; the sweep took %d, want 1",
			before, after, before-after)
	}

	if _, found, err := cache.GetAnyAge(ctx, fallback); err != nil || !found {
		t.Fatalf("eviction took a row the outage fallback still serves: found=%v err=%v", found, err)
	}
	if _, found, err := cache.GetAnyAge(ctx, ancient); err != nil || found {
		t.Fatalf("a row past the horizon survived eviction: found=%v err=%v", found, err)
	}
}

func backdate(ctx context.Context, t *testing.T, query, age string) {
	t.Helper()
	if _, err := testDBPool.Exec(ctx,
		`UPDATE artist_search_cache SET fetched_at = now() - $2::interval WHERE query = $1`,
		query, age); err != nil {
		t.Fatalf("backdate %q: %v", query, err)
	}
}

func TestSearchCacheRejectsEmptyQuery(t *testing.T) {
	ctx, _ := testTx(t)
	if testDBPool == nil {
		t.Skip("no database")
	}
	cache := storage.NewSearchCache(testDBPool, time.Hour)

	if _, err := cache.Put(ctx, "   ", []byte(`[]`)); err == nil {
		t.Fatal("Put accepted a whitespace-only query")
	}
	if _, found, err := cache.Get(ctx, ""); err != nil || found {
		t.Fatalf("Get(empty) = found %v, err %v", found, err)
	}
	// A short or malformed hash is an old button, not a database question.
	for _, bad := range []string{"", "abc", "abcdef1234567890"} {
		if _, found, err := cache.GetByHash(ctx, bad); err != nil || found {
			t.Fatalf("GetByHash(%q) = found %v, err %v", bad, found, err)
		}
	}
}
