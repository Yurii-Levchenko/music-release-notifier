package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

const (
	relChatA int64 = -999000010
	relChatB int64 = -999000011

	relArtist = "dddddddd-0000-4000-8000-000000000010"
	relGroup  = "eeeeeeee-0000-4000-8000-000000000020"
	relGroup2 = "ffffffff-0000-4000-8000-000000000021"
)

func testReleases(t *testing.T) (context.Context, *storage.Releases, *storage.Subscriptions) {
	t.Helper()
	if testDBPool == nil {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}
	ctx := context.Background()

	cleanup := func() {
		// Users cascade to subscriptions and notifications; releases and
		// artists are removed explicitly because nothing owns them.
		for _, chat := range []int64{relChatA, relChatB} {
			_, _ = testDBPool.Exec(ctx, `DELETE FROM users WHERE telegram_chat_id = $1`, chat)
		}
		_, _ = testDBPool.Exec(ctx,
			`DELETE FROM releases WHERE release_group_mbid IN ($1, $2)`, relGroup, relGroup2)
		_, _ = testDBPool.Exec(ctx, `DELETE FROM artists WHERE mbid = $1`, relArtist)
	}
	cleanup()
	t.Cleanup(cleanup)

	return ctx, storage.NewReleases(testDBPool), storage.NewSubscriptions(testDBPool)
}

func newRelease(group string) storage.NewRelease {
	return storage.NewRelease{
		ReleaseGroupMBID: group,
		ArtistMBID:       relArtist,
		Title:            "A Moon Shaped Pool",
		PrimaryType:      "Album",
		ReleaseDate:      time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
		CoverURL:         "https://coverartarchive.org/release/x/front-250",
	}
}

func countNotifications(ctx context.Context, t *testing.T, chatID int64) int {
	t.Helper()
	var n int
	if err := testDBPool.QueryRow(ctx, `
		SELECT count(*) FROM notifications n
		JOIN users u ON u.id = n.user_id
		WHERE u.telegram_chat_id = $1`, chatID).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// The core of S4: a new release reaches everyone subscribed to that artist, and
// only them.
func TestRecordFansOutToSubscribers(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	artist := storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}

	for _, chat := range []int64{relChatA, relChatB} {
		if _, err := subs.Subscribe(ctx, chat, artist, "bot"); err != nil {
			t.Fatalf("subscribe %d: %v", chat, err)
		}
	}

	out, err := releases.Record(ctx, newRelease(relGroup), true)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !out.Created {
		t.Fatal("Created=false for a release never seen before")
	}
	if out.Notified != 2 {
		t.Fatalf("notified %d, want 2", out.Notified)
	}
	for _, chat := range []int64{relChatA, relChatB} {
		if n := countNotifications(ctx, t, chat); n != 1 {
			t.Fatalf("chat %d has %d notifications, want 1", chat, n)
		}
	}
}

// The poll window is wider than the poll interval on purpose (SPEC.md D15), so
// every poll re-sees the same releases. Re-seeing one must be free and silent —
// otherwise the design that makes missed polls harmless would spam instead.
func TestRecordIsIdempotentAcrossOverlappingPolls(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	if _, err := subs.Subscribe(ctx, relChatA,
		storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	first, err := releases.Record(ctx, newRelease(relGroup), true)
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if !first.Created || first.Notified != 1 {
		t.Fatalf("first record: created=%v notified=%d", first.Created, first.Notified)
	}

	// Same release group, different title — a re-upload or a deluxe edition.
	// MusicBrainz release groups already merge editions (SPEC.md D7).
	second := newRelease(relGroup)
	second.Title = "A Moon Shaped Pool (Deluxe)"
	repeat, err := releases.Record(ctx, second, true)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if repeat.Created {
		t.Fatal("a known release group was recorded again")
	}
	if repeat.Notified != 0 {
		t.Fatalf("a repeat sighting queued %d notifications", repeat.Notified)
	}
	if n := countNotifications(ctx, t, relChatA); n != 1 {
		t.Fatalf("subscriber has %d notifications after two polls, want 1", n)
	}
}

// The first-run seed. Without it, day one would send a week of back catalog
// to everyone.
func TestRecordWithoutNotifyStoresButDoesNotQueue(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	if _, err := subs.Subscribe(ctx, relChatA,
		storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	out, err := releases.Record(ctx, newRelease(relGroup), false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !out.Created {
		t.Fatal("the seed did not store the release")
	}
	if out.Notified != 0 {
		t.Fatalf("the seed queued %d notifications", out.Notified)
	}
	if n := countNotifications(ctx, t, relChatA); n != 0 {
		t.Fatalf("subscriber has %d notifications after a seed", n)
	}

	// And the baseline must count as seen, so the next poll notifies normally.
	seen, err := releases.HasAnyReleases(ctx)
	if err != nil || !seen {
		t.Fatalf("HasAnyReleases = %v, err = %v", seen, err)
	}
}

// Somebody who subscribes tomorrow signed up for what comes next, not for a
// backlog. Fan-out happens once, to whoever is subscribed at that moment.
func TestRecordDoesNotReachLaterSubscribers(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	artist := storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}

	if _, err := subs.Subscribe(ctx, relChatA, artist, "bot"); err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	if _, err := releases.Record(ctx, newRelease(relGroup), true); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// B arrives after the release was already announced.
	if _, err := subs.Subscribe(ctx, relChatB, artist, "bot"); err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	if n := countNotifications(ctx, t, relChatB); n != 0 {
		t.Fatalf("a later subscriber received %d notifications for an older release", n)
	}
}

// A blocked user keeps their queued notification. A block is reversible, and
// the notifier skips them at send time — deleting the row would lose the
// release permanently if they came back.
func TestRecordStillQueuesForBlockedUsers(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	users := storage.NewUsers(testDBPool)

	if _, err := subs.Subscribe(ctx, relChatA,
		storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := users.SetBlocked(ctx, relChatA, true); err != nil {
		t.Fatalf("block: %v", err)
	}

	out, err := releases.Record(ctx, newRelease(relGroup), true)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if out.Notified != 1 {
		t.Fatalf("notified %d for a blocked subscriber, want 1", out.Notified)
	}
}

func TestRecordRejectsIncompleteReleases(t *testing.T) {
	ctx, releases, _ := testReleases(t)

	missingGroup := newRelease("")
	if _, err := releases.Record(ctx, missingGroup, true); err == nil {
		t.Fatal("accepted a release with no release group")
	}
	missingArtist := newRelease(relGroup)
	missingArtist.ArtistMBID = ""
	if _, err := releases.Record(ctx, missingArtist, true); err == nil {
		t.Fatal("accepted a release with no artist")
	}
}

// The poller matches against artists somebody follows, not against every artist
// a search ever created a row for.
func TestTrackedArtistMBIDsOnlyIncludesSubscribed(t *testing.T) {
	ctx, releases, subs := testReleases(t)

	// An artist row with no subscription, as a search would leave behind.
	if _, err := testDBPool.Exec(ctx,
		`INSERT INTO artists (mbid, name) VALUES ($1, 'Nobody Follows Me')
		 ON CONFLICT (mbid) DO NOTHING`, relArtist); err != nil {
		t.Fatalf("seed artist: %v", err)
	}

	tracked, err := releases.TrackedArtistMBIDs(ctx)
	if err != nil {
		t.Fatalf("TrackedArtistMBIDs: %v", err)
	}
	if _, ok := tracked[relArtist]; ok {
		t.Fatal("an artist nobody follows is being tracked")
	}

	if _, err := subs.Subscribe(ctx, relChatA,
		storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	tracked, err = releases.TrackedArtistMBIDs(ctx)
	if err != nil {
		t.Fatalf("TrackedArtistMBIDs: %v", err)
	}
	if _, ok := tracked[relArtist]; !ok {
		t.Fatal("a followed artist is missing from the tracked set")
	}
}

// The gap between last_polled_at and last_ok_at is what an alert watches, so a
// failure must move one and not the other.
func TestPollStateSeparatesAttemptFromSuccess(t *testing.T) {
	ctx, releases, _ := testReleases(t)
	const source = "test-source"
	t.Cleanup(func() {
		_, _ = testDBPool.Exec(ctx, `DELETE FROM poll_state WHERE source = $1`, source)
	})

	if _, found, err := releases.PollState(ctx, source); err != nil || found {
		t.Fatalf("state before any poll: found=%v err=%v", found, err)
	}

	if err := releases.RecordPoll(ctx, source, nil); err != nil {
		t.Fatalf("record success: %v", err)
	}
	ok, found, err := releases.PollState(ctx, source)
	if err != nil || !found {
		t.Fatalf("state after success: found=%v err=%v", found, err)
	}
	if ok.LastOKAt.IsZero() || ok.LastError != "" {
		t.Fatalf("after a success: last_ok=%v err=%q", ok.LastOKAt, ok.LastError)
	}

	failedAt := ok.LastOKAt
	if err := releases.RecordPoll(ctx, source, context.DeadlineExceeded); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	bad, _, err := releases.PollState(ctx, source)
	if err != nil {
		t.Fatalf("state after failure: %v", err)
	}
	if bad.LastError == "" {
		t.Fatal("a failed poll recorded no error")
	}
	if !bad.LastOKAt.Equal(failedAt) {
		t.Fatalf("a failed poll moved last_ok_at from %v to %v", failedAt, bad.LastOKAt)
	}
	if !bad.LastPolledAt.After(failedAt) && !bad.LastPolledAt.Equal(failedAt) {
		t.Fatal("a failed poll did not move last_polled_at")
	}
}

// The composite key is the second line of defense behind release groups, so it
// has to actually collapse the variants it claims to.
func TestDedupKeyNormalizesTitles(t *testing.T) {
	base := newRelease(relGroup)

	variants := []string{
		"A Moon Shaped Pool",
		"a moon shaped pool",
		"  A   Moon  Shaped   Pool  ",
		"A Moon-Shaped Pool!",
		"A. Moon, Shaped: Pool",
	}
	want := storage.DedupKey(base)
	for _, title := range variants {
		v := base
		v.Title = title
		if got := storage.DedupKey(v); got != want {
			t.Fatalf("DedupKey(%q) = %q, want %q", title, got, want)
		}
	}

	// Genuinely different releases must not collide.
	other := base
	other.Title = "In Rainbows"
	if storage.DedupKey(other) == want {
		t.Fatal("two different albums produced the same dedup key")
	}
	sameTitleLaterDate := base
	sameTitleLaterDate.ReleaseDate = base.ReleaseDate.AddDate(1, 0, 0)
	if storage.DedupKey(sameTitleLaterDate) == want {
		t.Fatal("a re-release a year later produced the same dedup key")
	}
}
