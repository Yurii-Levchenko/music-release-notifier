package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

type fakeSearcher struct {
	calls   int
	artists []musicbrainz.Artist
	err     error
}

func (f *fakeSearcher) SearchArtist(context.Context, string, int) ([]musicbrainz.Artist, error) {
	f.calls++
	return f.artists, f.err
}

type fakeCache struct {
	// fresh is what Get returns; any is what GetAnyAge returns. Separate so a
	// test can model the case that matters most: nothing fresh, something stale.
	fresh      storage.CachedSearch
	freshFound bool
	any        storage.CachedSearch
	anyFound   bool

	getErr    error
	anyErr    error
	putErr    error
	putCalls  int
	anyCalls  int
	freshCall int
}

func (f *fakeCache) Get(context.Context, string) (storage.CachedSearch, bool, error) {
	f.freshCall++
	return f.fresh, f.freshFound, f.getErr
}

func (f *fakeCache) GetAnyAge(context.Context, string) (storage.CachedSearch, bool, error) {
	f.anyCalls++
	return f.any, f.anyFound, f.anyErr
}

func (f *fakeCache) GetByHash(context.Context, string) (storage.CachedSearch, bool, error) {
	return f.fresh, f.freshFound, f.getErr
}

func (f *fakeCache) Put(_ context.Context, query string, payload []byte) (string, error) {
	f.putCalls++
	if f.putErr != nil {
		return "", f.putErr
	}
	f.fresh = storage.CachedSearch{Query: query, Hash: storage.HashQuery(query), Payload: payload}
	f.freshFound = true
	return f.fresh.Hash, nil
}

func (f *fakeCache) HashQuery(q string) string { return storage.HashQuery(q) }

func testBot(search ArtistSearcher, cache SearchCache) *Bot {
	return &Bot{search: search, cache: cache, log: discardLogger()}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var oneArtist = []musicbrainz.Artist{{MBID: "a1b2c3d4-0000-0000-0000-00000000000a", Name: "Radiohead"}}

func mustPayload(t *testing.T, artists []musicbrainz.Artist) []byte {
	t.Helper()
	b, err := json.Marshal(artists)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A fresh hit must answer without spending a MusicBrainz slot — the whole point
// of the cache, given the 1 req/s limit.
func TestFromCacheHit(t *testing.T) {
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{
		freshFound: true,
		fresh: storage.CachedSearch{
			Query: "radiohead", Hash: "abcdef123456", Payload: mustPayload(t, oneArtist),
		},
	}
	b := testBot(search, cache)

	artists, hash, ok := b.fromCache(context.Background(), "radiohead", discardLogger())
	if !ok || hash != "abcdef123456" || len(artists) != 1 {
		t.Fatalf("fromCache = (%d artists, %q, %v)", len(artists), hash, ok)
	}
	if search.calls != 0 {
		t.Fatalf("cache hit still called upstream %d times", search.calls)
	}
}

// A cache that errors, and a payload that no longer decodes, must both look like
// a plain miss. Either one breaking search would be worse than a slow search.
func TestFromCacheDegradesToMiss(t *testing.T) {
	for name, cache := range map[string]*fakeCache{
		"read error": {getErr: errors.New("database on fire")},
		"undecodable": {
			freshFound: true,
			fresh:      storage.CachedSearch{Payload: []byte(`{"not":"an array"}`)},
		},
		"not found": {freshFound: false},
	} {
		t.Run(name, func(t *testing.T) {
			b := testBot(&fakeSearcher{artists: oneArtist}, cache)
			if _, _, ok := b.fromCache(context.Background(), "radiohead", discardLogger()); ok {
				t.Fatal("fromCache reported a hit")
			}
		})
	}
}

func TestFetchStoresAndReportsFresh(t *testing.T) {
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{}
	b := testBot(search, cache)

	artists, hash, stale, err := b.fetchOrStale(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("fetchOrStale: %v", err)
	}
	if stale {
		t.Fatal("a successful fetch was reported as stale")
	}
	if len(artists) != 1 || hash != storage.HashQuery("radiohead") {
		t.Fatalf("artists=%d hash=%q", len(artists), hash)
	}
	if cache.putCalls != 1 {
		t.Fatalf("result was not cached: %d Put calls", cache.putCalls)
	}
}

// The reason this change exists. MusicBrainz 503s under load, and an expired
// list of artist names is enormously more useful than an apology — artist
// identities do not change week to week.
func TestFetchFallsBackToStaleCache(t *testing.T) {
	search := &fakeSearcher{err: errors.New("musicbrainz: giving up after 5 attempts")}
	cache := &fakeCache{
		freshFound: false, // nothing fresh
		anyFound:   true,  // but something old
		any: storage.CachedSearch{
			Query:     "radiohead",
			Hash:      "abcdef123456",
			Payload:   mustPayload(t, oneArtist),
			FetchedAt: time.Now().Add(-30 * 24 * time.Hour),
		},
	}
	b := testBot(search, cache)

	artists, hash, stale, err := b.fetchOrStale(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("upstream failure was not absorbed by the stale fallback: %v", err)
	}
	if !stale {
		t.Fatal("stale results were not labeled stale; the user would see old data as current")
	}
	if len(artists) != 1 || hash != "abcdef123456" {
		t.Fatalf("artists=%d hash=%q", len(artists), hash)
	}
	if cache.anyCalls != 1 {
		t.Fatalf("GetAnyAge called %d times, want 1", cache.anyCalls)
	}
}

// Nothing upstream and nothing cached is a genuine dead end, and must surface as
// an error rather than as an empty result set — "no artists found" would be a
// lie about a service outage.
func TestFetchFailsWhenNothingCached(t *testing.T) {
	wantErr := errors.New("musicbrainz: http 503")
	b := testBot(&fakeSearcher{err: wantErr}, &fakeCache{anyFound: false})

	_, _, stale, err := b.fetchOrStale(context.Background(), "radiohead", discardLogger())
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the upstream error", err)
	}
	if stale {
		t.Fatal("reported stale with nothing to serve")
	}
}

// A stale entry that no longer decodes is no better than none.
func TestFetchDoesNotServeUndecodableStale(t *testing.T) {
	wantErr := errors.New("musicbrainz: http 503")
	b := testBot(&fakeSearcher{err: wantErr}, &fakeCache{
		anyFound: true,
		any:      storage.CachedSearch{Payload: []byte("not json")},
	})

	if _, _, _, err := b.fetchOrStale(context.Background(), "radiohead", discardLogger()); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the upstream error", err)
	}
}

// An empty query cannot be answered by anything, so it must not spend a
// database round trip looking for a cached answer.
func TestFetchSkipsStaleLookupForEmptyQuery(t *testing.T) {
	cache := &fakeCache{anyFound: true, any: storage.CachedSearch{Payload: mustPayload(t, oneArtist)}}
	b := testBot(&fakeSearcher{err: musicbrainz.ErrEmptyQuery}, cache)

	if _, _, _, err := b.fetchOrStale(context.Background(), "   ", discardLogger()); !errors.Is(err, musicbrainz.ErrEmptyQuery) {
		t.Fatalf("err = %v, want ErrEmptyQuery", err)
	}
	if cache.anyCalls != 0 {
		t.Fatalf("looked for stale results for an empty query (%d calls)", cache.anyCalls)
	}
}

// A failed cache write must not lose the card: the buttons still need a handle,
// and it is derived from the query rather than from the write.
func TestFetchSurvivesFailedCacheWrite(t *testing.T) {
	b := testBot(&fakeSearcher{artists: oneArtist}, &fakeCache{putErr: errors.New("disk full")})

	artists, hash, stale, err := b.fetchOrStale(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("a failed cache write broke the search: %v", err)
	}
	if stale || len(artists) != 1 {
		t.Fatalf("stale=%v artists=%d", stale, len(artists))
	}
	if hash != storage.HashQuery("radiohead") {
		t.Fatalf("hash = %q, want one derived from the query", hash)
	}
}
