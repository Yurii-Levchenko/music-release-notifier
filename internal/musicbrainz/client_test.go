package musicbrainz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New("ReleaseRadar/test ( test@example.com )", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.baseURL = srv.URL
	// Tests must not spend real seconds waiting on the production limiter.
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	return c
}

// escapeLucene is the only piece of pure logic here, and it is the one that
// silently ruins search results when wrong: an unescaped ":" turns a name into
// a field query. Verified live that MusicBrainz parses Lucene from user input.
func TestEscapeLucene(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Real artist names must survive with every character intact.
		{"slash", "AC/DC", `AC\/DC`},
		{"bang", "Panic! At The Disco", `Panic\! At The Disco`},
		{"colon and bang", "Godspeed You! Black Emperor", `Godspeed You\! Black Emperor`},
		{"parens", "Sunn O)))", `Sunn O\)\)\)`},
		{"plus", "+/-", `\+\/\-`},
		{"question", "Sam Smith?", `Sam Smith\?`},

		// Non-ASCII is not syntax and must pass through untouched.
		{"accents", "Sigur Rós", "Sigur Rós"},
		{"cyrillic", "Океан Ельзи", "Океан Ельзи"},
		{"japanese", "宇多田ヒカル", "宇多田ヒカル"},

		// Lucene syntax becomes literal text.
		{"field query", "artist:radiohead", `artist\:radiohead`},
		{"brackets", "][", `\]\[`},
		{"wildcard", "radio*", `radio\*`},
		{"boolean chars", "foo && bar", `foo \&\& bar`},
		{"quote", `The "Who"`, `The \"Who\"`},
		{"backslash", `a\b`, `a\\b`},

		// Whitespace hygiene: pasted input often carries tabs and newlines.
		{"collapse spaces", "  radio   head  ", "radio head"},
		{"tabs and newlines", "radio\t\nhead", "radio head"},
		{"only whitespace", "   \t\n ", ""},
		{"empty", "", ""},
		{"control chars stripped", "radio\x00\x07head", "radiohead"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeLucene(tc.in); got != tc.want {
				t.Fatalf("escapeLucene(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSearchArtistRejectsEmptyQuery(t *testing.T) {
	var called atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	for _, q := range []string{"", "   ", "\t\n"} {
		_, err := c.SearchArtist(context.Background(), q, 5)
		if !errors.Is(err, ErrEmptyQuery) {
			t.Fatalf("SearchArtist(%q) error = %v, want ErrEmptyQuery", q, err)
		}
	}
	// MusicBrainz answers a blank query with 400; we must not spend a
	// rate-limited slot discovering that.
	if n := called.Load(); n != 0 {
		t.Fatalf("made %d HTTP calls for empty queries, want 0", n)
	}
}

const radioheadJSON = `{
  "count": 29,
  "artists": [
    {
      "id": "a74b1b7f-71a5-4011-9441-d0b5e4122711",
      "name": "Radiohead",
      "sort-name": "Radiohead",
      "score": 100,
      "type": "Group",
      "country": "GB",
      "life-span": {"begin": "1991"},
      "tags": [
        {"name": "rock", "count": 12},
        {"name": "alternative rock", "count": 20},
        {"name": "britpop", "count": 3},
        {"name": "experimental", "count": 7}
      ]
    },
    {
      "id": "b1c2d3e4-0000-0000-0000-000000000002",
      "name": "On a Friday",
      "score": 65,
      "type": "Group",
      "disambiguation": "pre-Radiohead group, until 1991",
      "life-span": {"begin": "1985", "end": "1991"}
    }
  ]
}`

func TestSearchArtistParsesResponse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		// The name must reach the API escaped, not raw.
		if got := r.URL.Query().Get("query"); got != "radiohead" {
			t.Errorf("query param = %q, want %q", got, "radiohead")
		}
		if got := r.URL.Query().Get("fmt"); got != "json" {
			t.Errorf("fmt param = %q, want json", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "@") {
			t.Errorf("User-Agent %q carries no contact address; MusicBrainz 503s on that", ua)
		}
		_, _ = w.Write([]byte(radioheadJSON))
	})

	got, err := c.SearchArtist(context.Background(), "radiohead", 5)
	if err != nil {
		t.Fatalf("SearchArtist: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d artists, want 2", len(got))
	}

	first := got[0]
	if first.MBID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" || first.Name != "Radiohead" {
		t.Fatalf("first artist = %+v", first)
	}
	if first.Score != 100 || first.Type != "Group" || first.Country != "GB" || first.Begin != "1991" {
		t.Fatalf("disambiguating fields lost: %+v", first)
	}
	// Tags must come back ordered by vote count, not document order.
	want := []string{"alternative rock", "rock", "experimental"}
	if len(first.Tags) != len(want) {
		t.Fatalf("tags = %v, want %v", first.Tags, want)
	}
	for i := range want {
		if first.Tags[i] != want[i] {
			t.Fatalf("tags = %v, want %v", first.Tags, want)
		}
	}
	if got[1].Disambiguation != "pre-Radiohead group, until 1991" || got[1].End != "1991" {
		t.Fatalf("second artist lost disambiguation: %+v", got[1])
	}
}

// Zero results is a normal 200 with count 0, not an error and not a 503.
func TestSearchArtistEmptyResultIsNotAnError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count":0,"artists":[]}`))
	})

	got, err := c.SearchArtist(context.Background(), "zzzznotanartist", 5)
	if err != nil {
		t.Fatalf("empty result returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d artists, want 0", len(got))
	}
}

// 503 is returned for rate limiting, for a missing User-Agent and for real load,
// all with the same generic body. Every 503 observed live resolved on retry, so
// it must be retried and must never be reported as "no results".
func TestSearchArtistRetries503(t *testing.T) {
	var attempts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"The MusicBrainz web server is currently busy."}`))
			return
		}
		_, _ = w.Write([]byte(radioheadJSON))
	})
	// Keep the test fast; the production backoff is seconds.
	c.limiter = rate.NewLimiter(rate.Inf, 1)

	got, err := c.SearchArtist(context.Background(), "radiohead", 5)
	if err != nil {
		t.Fatalf("SearchArtist after retries: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no artists after a successful retry")
	}
	if n := attempts.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestSearchArtistGivesUpAfterMaxAttempts(t *testing.T) {
	var attempts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := c.SearchArtist(ctx, "radiohead", 5); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if n := attempts.Load(); n != maxAttempts {
		t.Fatalf("attempts = %d, want %d", n, maxAttempts)
	}
}

// A 400 means the request itself is wrong. Retrying it just burns rate-limited
// slots against a server that has already given its answer.
func TestSearchArtistDoesNotRetry4xx(t *testing.T) {
	var attempts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid query"}`))
	})

	if _, err := c.SearchArtist(context.Background(), "radiohead", 5); err == nil {
		t.Fatal("expected an error on 400")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1 — 4xx must not be retried", n)
	}
}

func TestSearchArtistHonorsContextCancellation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.SearchArtist(ctx, "radiohead", 5); err == nil {
		t.Fatal("expected an error when the context is already canceled")
	}
}

// The rate limiter is the thing standing between this bot and a blocked IP, so
// assert it actually gates requests rather than trusting the constant.
func TestRateLimiterSerializesRequests(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count":0,"artists":[]}`))
	})
	// 20/s so three requests take ~100ms rather than ~2s.
	c.limiter = rate.NewLimiter(20, 1)

	start := time.Now()
	for i := range 3 {
		if _, err := c.SearchArtist(context.Background(), "query", 5); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// Three requests at 20/s with burst 1: the first is free, the next two wait
	// ~50ms each.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("3 requests took %v — the limiter is not gating anything", elapsed)
	}
}

func TestSearchArtistClampsLimit(t *testing.T) {
	var seen string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"count":0,"artists":[]}`))
	})

	for _, bad := range []int{0, -1, 1000} {
		if _, err := c.SearchArtist(context.Background(), "x", bad); err != nil {
			t.Fatalf("limit %d: %v", bad, err)
		}
		if seen != "5" {
			t.Fatalf("limit %d was sent as %q, want the clamped default 5", bad, seen)
		}
	}
}

func TestSearchArtistRejectsGarbageJSON(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	})

	if _, err := c.SearchArtist(context.Background(), "radiohead", 5); err == nil {
		t.Fatal("expected a decode error for a non-JSON body")
	}
}

// A missing or generic User-Agent cannot succeed: MusicBrainz answers an absent
// one with 403 ("the application you are using has not identified itself") and
// a generic one with a vague 503. Both are unrecoverable, so the constructor
// refuses rather than letting every search fail one request at a time.
func TestNewRequiresIdentifyingUserAgent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, bad := range []string{"", "   ", "Go-http-client/1.1", "curl/8.1.2", "ReleaseRadar/0.1"} {
		if _, err := New(bad, log); !errors.Is(err, ErrNoUserAgent) {
			t.Fatalf("New(%q) error = %v, want ErrNoUserAgent", bad, err)
		}
	}

	for _, good := range []string{
		"ReleaseRadar/0.1 ( me@example.com )",
		"ReleaseRadar/0.1 (+https://example.com; me@example.com)",
	} {
		if _, err := New(good, log); err != nil {
			t.Fatalf("New(%q) = %v, want nil", good, err)
		}
	}
}

// MusicBrainz editors leave notes to each other as ordinary tags, and those
// notes get voted on — so they outrank real genres and land on the card. Seen
// live: あいみょん came back as "singer-songwriter · j-pop · fixme label mess".
func TestTopTagsDropsEditorNotes(t *testing.T) {
	type mbTag = struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	tags := []mbTag{
		{"fixme label mess", 9}, // most-voted, and pure noise to a reader
		{"j-pop", 5},
		{"singer-songwriter", 4},
		{"needs splitting", 3},
		{"city pop", 2},
	}

	got := topTags(tags)
	want := []string{"j-pop", "singer-songwriter", "city pop"}

	if len(got) != len(want) {
		t.Fatalf("topTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("topTags = %v, want %v", got, want)
		}
	}
}

// Filtering must not invent a floor: an artist whose only tags are notes shows
// no tags, which is correct — the card has other fields.
func TestTopTagsSurvivesAllNoise(t *testing.T) {
	type mbTag = struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	if got := topTags([]mbTag{{"fixme", 3}, {"todo: verify", 1}}); len(got) != 0 {
		t.Fatalf("topTags = %v, want none", got)
	}
}

// A genre must not be filtered because a marker appears mid-word.
func TestTopTagsKeepsRealGenres(t *testing.T) {
	type mbTag = struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}

	for _, genre := range []string{"post-rock", "checkered pop", "spamdexcore"} {
		got := topTags([]mbTag{{genre, 5}})
		if len(got) != 1 || got[0] != genre {
			t.Errorf("topTags dropped %q: %v", genre, got)
		}
	}
}
