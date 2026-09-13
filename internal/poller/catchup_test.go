package poller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
)

type fakeCatchUpStore struct {
	queued []string
	err    error
}

func (f *fakeCatchUpStore) QueueCatchUp(_ context.Context, _ int64, rg string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.queued = append(f.queued, rg)
	return true, nil
}

const catchUpChat int64 = 555

func catchUpPoller(feed Feed, store Store, cu CatchUpStore) *Poller {
	p := New(feed, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = func() time.Time { return fixedNow }
	return p.WithCatchUp(cu)
}

func recent(rg, artist string, daysAgo int) listenbrainz.Release {
	return listenbrainz.Release{
		ReleaseGroupMBID: rg,
		ArtistMBIDs:      []string{artist},
		ArtistName:       "Somebody",
		Title:            "A Record",
		PrimaryType:      "Album",
		ReleaseDate:      fixedNow.AddDate(0, 0, -daysAgo),
	}
}

// The point of the feature: subscribing to an artist who released something on
// Friday should be worth doing on Sunday, not only from the next poll onward.
func TestCatchUpQueuesAKnownRecentRelease(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000001"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-1", artist, 2)}}
	// Already recorded by an earlier poll, and fanned out then — without this
	// subscriber, who signed up afterwards. That is the case the catch-up
	// exists for.
	store := &fakeStore{
		tracked: map[string]struct{}{artist: {}},
		known:   map[string]bool{"rg-1": true},
	}
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(feed, store, cu).CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued %d, want 1", queued)
	}
	if len(cu.queued) != 1 || cu.queued[0] != "rg-1" {
		t.Fatalf("queued = %v", cu.queued)
	}
}

// The hazard this design has to avoid. A release nobody has recorded yet must
// be fanned out to every subscriber, not just the new one: the poller skips a
// release group it has already seen, so recording it here without a fan-out
// would mean everybody else never hears about it at all.
func TestCatchUpFansOutAReleaseNobodyHasRecorded(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000002"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-new", artist, 1)}}
	store := &fakeStore{tracked: map[string]struct{}{artist: {}}}
	cu := &fakeCatchUpStore{}

	if _, err := catchUpPoller(feed, store, cu).CatchUp(context.Background(), catchUpChat, artist); err != nil {
		t.Fatalf("catch up: %v", err)
	}

	if len(store.records) != 1 {
		t.Fatalf("recorded %d releases, want 1", len(store.records))
	}
	if !store.records[0].notify {
		t.Fatal("recorded without a fan-out; every other subscriber would never hear about this release")
	}
	if len(cu.queued) != 0 {
		t.Fatal("also queued a catch-up row for a release the fan-out already covered")
	}
}

// Nothing recent is the common case and must be silent and cheap.
func TestCatchUpWithNothingRecent(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000003"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-old", artist, 30)}}
	store := &fakeStore{}
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(feed, store, cu).CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 0 || len(cu.queued) != 0 || len(store.records) != 0 {
		t.Fatalf("queued %d, rows %v, records %d", queued, cu.queued, len(store.records))
	}
}

// Another artist's release in the same window must not leak into this
// subscription.
func TestCatchUpIgnoresOtherArtists(t *testing.T) {
	mine := "aaaa1111-0000-4000-8000-000000000004"
	theirs := "bbbb2222-0000-4000-8000-000000000004"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-theirs", theirs, 1)}}
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(feed, &fakeStore{}, cu).CatchUp(context.Background(), catchUpChat, mine)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 0 {
		t.Fatalf("queued somebody else's release: %v", cu.queued)
	}
}

// The same filter the poll uses, from the same definition — a compilation is
// not a new release however it arrives.
func TestCatchUpAppliesTheTypeFilter(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000005"
	rel := recent("rg-comp", artist, 1)
	rel.SecondaryType = "Compilation"
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(&fakeFeed{releases: []listenbrainz.Release{rel}},
		&fakeStore{}, cu).CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 0 {
		t.Fatal("a compilation was caught up")
	}
}

// Announcing an album before it exists is the one mistake a release bot cannot
// walk back, and a courtesy path is no exception.
func TestCatchUpSkipsFutureDates(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000006"
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(&fakeFeed{releases: []listenbrainz.Release{recent("rg-future", artist, -2)}},
		&fakeStore{}, cu).CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 0 {
		t.Fatal("a future-dated release was caught up")
	}
}

// Anything older than the window belongs to the past, not to a welcome
// message. Seven days is inside the poll window and outside this one.
func TestCatchUpWindowIsShorterThanThePollWindow(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-00000000000a"
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(&fakeFeed{releases: []listenbrainz.Release{recent("rg-week", artist, 6)}},
		&fakeStore{}, cu).CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued != 0 {
		t.Fatalf("a %d-day-old release was caught up; the window is %d days", 6, catchUpDays)
	}
}

// Pressing one button must not produce an unbounded burst.
func TestCatchUpIsCapped(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000007"
	var releases []listenbrainz.Release
	for i := range 6 {
		releases = append(releases, recent(fmt.Sprintf("rg-many-%d", i), artist, 1))
	}
	cu := &fakeCatchUpStore{}

	queued, err := catchUpPoller(&fakeFeed{releases: releases},
		&fakeStore{tracked: map[string]struct{}{artist: {}}}, cu).
		CatchUp(context.Background(), catchUpChat, artist)
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if queued > maxCatchUp {
		t.Fatalf("queued %d, want at most %d", queued, maxCatchUp)
	}
}

// Subscribing to several artists in a row must not refetch a 360 KB window
// each time.
func TestCatchUpReusesTheFeedWindow(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000008"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-x", artist, 1)}}
	p := catchUpPoller(feed, &fakeStore{}, &fakeCatchUpStore{})

	for range 3 {
		if _, err := p.CatchUp(context.Background(), catchUpChat, artist); err != nil {
			t.Fatalf("catch up: %v", err)
		}
	}
	if feed.calls != 1 {
		t.Fatalf("fetched the feed %d times, want 1", feed.calls)
	}
	// Three days, not the poll's seven: this window answers a different
	// question and fetching the wider one would be wasted bytes.
	if feed.gotDays != catchUpDays {
		t.Fatalf("asked for %d days, want %d", feed.gotDays, catchUpDays)
	}
}

// A feed outage must not make a successful subscription look broken, and a
// slightly stale answer to "did they release something this week" beats none.
func TestCatchUpServesAStaleWindowOnFailure(t *testing.T) {
	artist := "aaaa1111-0000-4000-8000-000000000009"
	feed := &fakeFeed{releases: []listenbrainz.Release{recent("rg-y", artist, 1)}}
	p := catchUpPoller(feed, &fakeStore{}, &fakeCatchUpStore{})

	if _, err := p.CatchUp(context.Background(), catchUpChat, artist); err != nil {
		t.Fatalf("first catch up: %v", err)
	}

	// Expire the cache, then break the feed.
	p.feed3.fetched = fixedNow.Add(-2 * feedCacheTTL)
	feed.err = errors.New("503 service unavailable")

	if _, err := p.CatchUp(context.Background(), catchUpChat, artist); err != nil {
		t.Fatalf("a feed outage propagated to the caller: %v", err)
	}
}

// Without the store wired, this must be inert rather than a nil dereference:
// nil is the normal state for every existing test and for a build with the
// feature switched off.
func TestCatchUpWithoutAStoreIsInert(t *testing.T) {
	feed := &fakeFeed{}
	p := New(feed, &fakeStore{}, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	queued, err := p.CatchUp(context.Background(), catchUpChat, "whatever")
	if err != nil || queued != 0 {
		t.Fatalf("queued %d, err %v", queued, err)
	}
	if feed.calls != 0 {
		t.Fatal("fetched the feed with nowhere to put the result")
	}
}
