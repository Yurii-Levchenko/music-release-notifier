package poller

import (
	"context"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Belt and braces: even if a source ever hands us a date carrying a time, the
// guard must still classify it by calendar day.
func TestFutureGuardIgnoresTimeComponent(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-late-today", "Album", []string{tracked1}, fixedNow.Add(11*time.Hour)),
		release("rg-tomorrow", "Album", []string{tracked1}, day(1).Add(3*time.Hour)),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Recorded != 1 {
		t.Fatalf("recorded %d, want 1 — today with a time is still today", stats.Recorded)
	}
	if store.records[0].rel.ReleaseGroupMBID != "rg-late-today" {
		t.Fatalf("kept %q, want rg-late-today", store.records[0].rel.ReleaseGroupMBID)
	}
}
