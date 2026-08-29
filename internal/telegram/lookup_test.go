package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

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
	entry    storage.CachedSearch
	found    bool
	getErr   error
	putErr   error
	putCalls int
}

func (f *fakeCache) Get(context.Context, string) (storage.CachedSearch, bool, error) {
	return f.entry, f.found, f.getErr
}

func (f *fakeCache) GetByHash(context.Context, string) (storage.CachedSearch, bool, error) {
	return f.entry, f.found, f.getErr
}

func (f *fakeCache) Put(_ context.Context, query string, payload []byte) (string, error) {
	f.putCalls++
	if f.putErr != nil {
		return "", f.putErr
	}
	f.entry = storage.CachedSearch{Query: query, Hash: storage.HashQuery(query), Payload: payload}
	f.found = true
	return f.entry.Hash, nil
}

func (f *fakeCache) HashQuery(q string) string { return storage.HashQuery(q) }

func testBot(search ArtistSearcher, cache SearchCache) *Bot {
	return &Bot{search: search, cache: cache, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var oneArtist = []musicbrainz.Artist{{MBID: "a1b2c3d4-0000-0000-0000-00000000000a", Name: "Radiohead"}}

// The reported source is what makes cache behavior visible in the logs. Getting
// it wrong is invisible in the product and misleading in every later
// investigation, so it is worth asserting directly.
func TestLookupReportsUpstreamOnMiss(t *testing.T) {
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{found: false}
	b := testBot(search, cache)

	got, hash, source, err := b.lookup(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if source != sourceUpstream {
		t.Fatalf("source = %q, want %q", source, sourceUpstream)
	}
	if len(got) != 1 || search.calls != 1 {
		t.Fatalf("got %d artists after %d upstream calls", len(got), search.calls)
	}
	if hash != storage.HashQuery("radiohead") {
		t.Fatalf("hash = %q", hash)
	}
	if cache.putCalls != 1 {
		t.Fatalf("result was not cached: %d Put calls", cache.putCalls)
	}
}

func TestLookupReportsCacheOnHit(t *testing.T) {
	payload, err := json.Marshal(oneArtist)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{
		found: true,
		entry: storage.CachedSearch{Query: "radiohead", Hash: "abcdef123456", Payload: payload},
	}
	b := testBot(search, cache)

	got, hash, source, err := b.lookup(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if source != sourceCache {
		t.Fatalf("source = %q, want %q", source, sourceCache)
	}
	if hash != "abcdef123456" {
		t.Fatalf("hash = %q, want the cached one", hash)
	}
	if len(got) != 1 {
		t.Fatalf("got %d artists", len(got))
	}
	// The whole point of the cache: MusicBrainz is rate limited to 1 req/s, so a
	// hit must not spend a slot.
	if search.calls != 0 {
		t.Fatalf("cache hit still called upstream %d times", search.calls)
	}
}

// A cache that errors must slow the bot down, never break it.
func TestLookupSurvivesBrokenCache(t *testing.T) {
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{getErr: errors.New("database on fire"), putErr: errors.New("still on fire")}
	b := testBot(search, cache)

	got, hash, source, err := b.lookup(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("a broken cache broke the search: %v", err)
	}
	if source != sourceUpstream || len(got) != 1 {
		t.Fatalf("source = %q with %d artists", source, len(got))
	}
	// A failed write still has to yield a usable handle, or the card renders
	// with buttons that resolve to nothing.
	if hash != storage.HashQuery("radiohead") {
		t.Fatalf("hash = %q, want one derived from the query", hash)
	}
}

// A payload written by an older build may no longer decode. That must fall
// through to a fresh search rather than showing the user nothing.
func TestLookupFallsBackWhenCachedPayloadIsUndecodable(t *testing.T) {
	search := &fakeSearcher{artists: oneArtist}
	cache := &fakeCache{
		found: true,
		entry: storage.CachedSearch{Query: "radiohead", Hash: "abcdef123456", Payload: []byte(`{"not":"an array"}`)},
	}
	b := testBot(search, cache)

	got, _, source, err := b.lookup(context.Background(), "radiohead", discardLogger())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if source != sourceUpstream {
		t.Fatalf("source = %q, want a fresh upstream fetch", source)
	}
	if len(got) != 1 || search.calls != 1 {
		t.Fatalf("got %d artists after %d upstream calls", len(got), search.calls)
	}
}

func TestLookupPropagatesUpstreamFailure(t *testing.T) {
	wantErr := errors.New("musicbrainz: http 503")
	b := testBot(&fakeSearcher{err: wantErr}, &fakeCache{found: false})

	if _, _, _, err := b.lookup(context.Background(), "radiohead", discardLogger()); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the upstream error", err)
	}
}
