package listenbrainz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
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
	return c
}

// Shaped after a real response, including the fields that caused trouble:
// caa_id exceeds 32 bits, and the feed carries entries that cannot be used.
const feedJSON = `{
  "payload": {
    "total_count": 6,
    "releases": [
      {
        "artist_credit_name": "Actress",
        "artist_mbids": ["973321d1-9565-4214-a6ef-a77f1890c294"],
        "caa_id": 45986406826,
        "caa_release_mbid": "7097ffd2-d133-47cd-a6bb-3cf6a98755a3",
        "release_date": "2026-08-19",
        "release_group_mbid": "d631ddfc-b64f-4071-8846-345305cfb0af",
        "release_mbid": "7097ffd2-d133-47cd-a6bb-3cf6a98755a3",
        "release_name": "Withending",
        "release_group_primary_type": "Single"
      },
      {
        "artist_credit_name": "No Cover Band",
        "artist_mbids": ["aaaaaaaa-0000-4000-8000-000000000001"],
        "caa_id": 0,
        "caa_release_mbid": "",
        "release_date": "2026-08-20",
        "release_group_mbid": "bbbbbbbb-0000-4000-8000-000000000002",
        "release_mbid": "cccccccc-0000-4000-8000-000000000003",
        "release_name": "Coverless",
        "release_group_primary_type": "Album"
      },
      {
        "artist_credit_name": "Collaboration",
        "artist_mbids": ["dddddddd-0000-4000-8000-000000000004", "eeeeeeee-0000-4000-8000-000000000005"],
        "caa_id": 1,
        "caa_release_mbid": "ffffffff-0000-4000-8000-000000000006",
        "release_date": "2026-08-21",
        "release_group_mbid": "11111111-0000-4000-8000-000000000007",
        "release_mbid": "22222222-0000-4000-8000-000000000008",
        "release_name": "Together",
        "release_group_primary_type": "EP"
      },
      {
        "artist_credit_name": "No Release Group",
        "artist_mbids": ["33333333-0000-4000-8000-000000000009"],
        "release_date": "2026-08-21",
        "release_group_mbid": "",
        "release_name": "Unusable",
        "release_group_primary_type": "Album"
      },
      {
        "artist_credit_name": "No Artist",
        "artist_mbids": [],
        "release_date": "2026-08-21",
        "release_group_mbid": "44444444-0000-4000-8000-000000000010",
        "release_name": "Orphan",
        "release_group_primary_type": "Album"
      },
      {
        "artist_credit_name": "Bad Date",
        "artist_mbids": ["55555555-0000-4000-8000-000000000011"],
        "release_date": "2026-08",
        "release_group_mbid": "66666666-0000-4000-8000-000000000012",
        "release_name": "Imprecise",
        "release_group_primary_type": "Album"
      }
    ]
  }
}`

func TestFreshReleasesParsesAndFiltersUnusableEntries(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// days=N is a window of ±N days, not a look-back (SPEC.md C24). Without
		// these flags the poller would see releases three days into the future.
		if q.Get("past") != "true" || q.Get("future") != "false" {
			t.Errorf("past=%q future=%q, want true and false", q.Get("past"), q.Get("future"))
		}
		if q.Get("days") != "7" {
			t.Errorf("days=%q, want 7", q.Get("days"))
		}
		_, _ = w.Write([]byte(feedJSON))
	})

	got, err := c.FreshReleases(context.Background(), 7)
	if err != nil {
		t.Fatalf("FreshReleases: %v", err)
	}

	// Three of six survive: the others lack a release group, lack an artist, or
	// carry a date we cannot place on a day.
	if len(got) != 3 {
		names := make([]string, len(got))
		for i := range got {
			names[i] = got[i].Title
		}
		t.Fatalf("got %d usable releases %v, want 3", len(got), names)
	}

	first := got[0]
	if first.Title != "Withending" || first.PrimaryType != "Single" {
		t.Fatalf("first release = %+v", first)
	}
	if want := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC); !first.ReleaseDate.Equal(want) {
		t.Fatalf("release date = %v, want %v", first.ReleaseDate, want)
	}
	// The cover URL is assembled from the response, never fetched (SPEC.md C27).
	if want := "https://coverartarchive.org/release/7097ffd2-d133-47cd-a6bb-3cf6a98755a3/front-250"; first.CoverURL != want {
		t.Fatalf("cover URL = %q, want %q", first.CoverURL, want)
	}

	// Roughly a quarter of releases have no cover art (SPEC.md C27b), and that
	// has to come through as empty rather than as a broken URL.
	if got[1].CoverURL != "" {
		t.Fatalf("a release with no caa_id produced a cover URL: %q", got[1].CoverURL)
	}

	// A collaboration keeps every credited artist; the poller decides which one
	// it was subscribed to.
	if len(got[2].ArtistMBIDs) != 2 {
		t.Fatalf("collaboration kept %d artist ids, want 2", len(got[2].ArtistMBIDs))
	}
}

// caa_id is an Internet Archive identifier well past 32 bits. Parsed as an int
// on a 32-bit build, or as a float, it would silently produce the wrong URL.
func TestFreshReleasesHandlesLargeCAAID(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(feedJSON))
	})
	got, err := c.FreshReleases(context.Background(), 7)
	if err != nil {
		t.Fatalf("FreshReleases: %v", err)
	}
	if got[0].CoverURL == "" {
		t.Fatal("a release with caa_id 45986406826 produced no cover URL")
	}
}

func TestFreshReleasesRejectsBadWindow(t *testing.T) {
	var called atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	for _, days := range []int{0, -1, 91, 1000} {
		if _, err := c.FreshReleases(context.Background(), days); err == nil {
			t.Fatalf("days=%d was accepted", days)
		}
	}
	if n := called.Load(); n != 0 {
		t.Fatalf("made %d requests for out-of-range windows", n)
	}
}

func TestFreshReleasesRetriesServerErrors(t *testing.T) {
	var attempts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(feedJSON))
	})

	if _, err := c.FreshReleases(context.Background(), 7); err != nil {
		t.Fatalf("FreshReleases after a retry: %v", err)
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

// A 4xx means the request is wrong. Retrying it wastes a volunteer service's
// time on a question it has already answered.
func TestFreshReleasesDoesNotRetryClientErrors(t *testing.T) {
	var attempts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	if _, err := c.FreshReleases(context.Background(), 7); err == nil {
		t.Fatal("expected an error on 400")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

func TestFreshReleasesRejectsGarbage(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	})
	if _, err := c.FreshReleases(context.Background(), 7); err == nil {
		t.Fatal("expected a decode error")
	}
}

// An empty window is a normal answer, not a failure: a quiet week is possible,
// and treating it as an error would make the poller log noise every Sunday.
func TestFreshReleasesEmptyFeedIsNotAnError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"payload":{"total_count":0,"releases":[]}}`))
	})
	got, err := c.FreshReleases(context.Background(), 7)
	if err != nil {
		t.Fatalf("empty feed returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d releases from an empty feed", len(got))
	}
}

func TestNewRequiresIdentifyingUserAgent(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, bad := range []string{"", "   ", "Go-http-client/1.1", "ReleaseRadar/0.1"} {
		if _, err := New(bad, log); !errors.Is(err, ErrNoUserAgent) {
			t.Fatalf("New(%q) error = %v, want ErrNoUserAgent", bad, err)
		}
	}
	if _, err := New("ReleaseRadar/0.1 ( me@example.com )", log); err != nil {
		t.Fatalf("New with a valid UA: %v", err)
	}
}

func TestCoverURL(t *testing.T) {
	const mbid = "7097ffd2-d133-47cd-a6bb-3cf6a98755a3"
	if got := coverURL(mbid, 42); got == "" {
		t.Fatal("a release with cover art produced no URL")
	}
	// Either field missing means there is no art to point at, and a URL that
	// 404s would make Telegram reject the whole message.
	if got := coverURL("", 42); got != "" {
		t.Fatalf("coverURL with no mbid = %q", got)
	}
	if got := coverURL(mbid, 0); got != "" {
		t.Fatalf("coverURL with no caa_id = %q", got)
	}
}

func TestParseReleaseDate(t *testing.T) {
	if got, ok := parseReleaseDate("2026-08-19"); !ok ||
		!got.Equal(time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("parseReleaseDate = (%v, %v)", got, ok)
	}
	// MusicBrainz allows year- and month-only precision. A release we cannot
	// place on a day cannot be checked against "is it out yet", so it is skipped
	// rather than guessed at.
	for _, bad := range []string{"", "2026", "2026-08", "not a date", "19-08-2026"} {
		if _, ok := parseReleaseDate(bad); ok {
			t.Fatalf("parseReleaseDate(%q) reported success", bad)
		}
	}
}

// Every excluded kind, so adding one to the list cannot silently do nothing.
func TestExcludedSecondaryTypes(t *testing.T) {
	for _, kind := range []string{"Compilation", "Demo", "Interview", "Audiobook", "Spokenword", "DJ-mix"} {
		if (Release{SecondaryType: kind}).NotifiableSecondary() {
			t.Errorf("%q is notifiable; it is meant to be excluded", kind)
		}
	}
}

// The kept ones matter as much as the excluded ones: each is a real release
// somebody following the artist wants to hear about, and quietly dropping a
// live album or a mixtape would be worse than the noise this filter removes.
func TestKeptSecondaryTypes(t *testing.T) {
	for _, kind := range []string{"", "Live", "Remix", "Soundtrack", "Mixtape/Street"} {
		if !(Release{SecondaryType: kind}).NotifiableSecondary() {
			t.Errorf("%q was filtered out; it is meant to be kept", kind)
		}
	}
}

// An unfamiliar value must pass. The feed's vocabulary is MusicBrainz's, which
// grows, and defaulting to "drop" would make a new qualifier silently swallow
// releases.
func TestUnknownSecondaryTypePasses(t *testing.T) {
	if !(Release{SecondaryType: "Something MusicBrainz Added Later"}).NotifiableSecondary() {
		t.Fatal("an unrecognized secondary type was dropped")
	}
}

// Notifiable is both checks, and both have to apply — a Compilation Album must
// not pass because its primary type is fine.
func TestNotifiableNeedsBothChecks(t *testing.T) {
	if (Release{PrimaryType: "Album", SecondaryType: "Compilation"}).Notifiable() {
		t.Error("a compilation album passed the combined check")
	}
	if (Release{PrimaryType: "Broadcast"}).Notifiable() {
		t.Error("a broadcast passed the combined check")
	}
	if !(Release{PrimaryType: "Album"}).Notifiable() {
		t.Error("a plain album was filtered out")
	}
}
