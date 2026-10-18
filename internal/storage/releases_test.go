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

// A title-track single released the same day as its album is two release
// groups with one artist|title|date. Both must be recorded and both announced:
// the release group is the dedup key (D7), and the composite used to be UNIQUE
// too, which turned the second release into a 23505 that was logged daily and
// never delivered (migration 0009).
func TestSameTitleSameDayReleasesAreBothRecorded(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	if _, err := subs.Subscribe(ctx, relChatA,
		storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	single := newRelease(relGroup)
	single.PrimaryType = "Single"
	album := newRelease(relGroup2)
	if storage.DedupKey(single) != storage.DedupKey(album) {
		t.Fatal("fixture no longer reproduces the collision: the two releases must share a dedup key")
	}

	for _, rel := range []storage.NewRelease{single, album} {
		out, err := releases.Record(ctx, rel, true)
		if err != nil {
			t.Fatalf("Record %s %s: %v", rel.PrimaryType, rel.ReleaseGroupMBID, err)
		}
		if !out.Created || out.Notified != 1 {
			t.Fatalf("Record %s: created=%v notified=%d, want a new release announced once",
				rel.PrimaryType, out.Created, out.Notified)
		}
	}
	if n := countNotifications(ctx, t, relChatA); n != 2 {
		t.Fatalf("subscriber has %d notifications, want 2 — one per release group", n)
	}
}

// Blocking the bot means the user has left (SPEC D16). Their subscriptions go,
// whatever was queued for them is skipped rather than left pending forever,
// and later releases do not fan out to them. This used to keep all three, so
// the outcome depended on whether we heard about the block from Telegram's
// my_chat_member update or from a 403 on the next send — which deleted them.
func TestBlockingTheBotDropsTheUser(t *testing.T) {
	ctx, releases, subs := testReleases(t)
	users := storage.NewUsers(testDBPool)
	artist := storage.ArtistRef{MBID: relArtist, Name: "Radiohead"}

	for _, chat := range []int64{relChatA, relChatB} {
		if _, err := subs.Subscribe(ctx, chat, artist, "bot"); err != nil {
			t.Fatalf("subscribe %d: %v", chat, err)
		}
	}
	if _, err := releases.Record(ctx, newRelease(relGroup), true); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := users.SetBlocked(ctx, relChatA, true); err != nil {
		t.Fatalf("block: %v", err)
	}

	var subsLeft, pendingLeft int
	if err := testDBPool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM subscriptions s JOIN users u ON u.id = s.user_id
			 WHERE u.telegram_chat_id = $1),
			(SELECT count(*) FROM notifications n JOIN users u ON u.id = n.user_id
			 WHERE u.telegram_chat_id = $1 AND n.state = 'pending')`,
		relChatA).Scan(&subsLeft, &pendingLeft); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if subsLeft != 0 || pendingLeft != 0 {
		t.Fatalf("after blocking: %d subscriptions and %d pending rows left, want none", subsLeft, pendingLeft)
	}

	next := newRelease(relGroup2)
	next.Title = "Kid A" // a different release, so a different dedup key
	out, err := releases.Record(ctx, next, true)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if out.Notified != 1 {
		t.Fatalf("notified %d for the next release, want 1 — only the subscriber who stayed", out.Notified)
	}

	// Someone else's block must not touch this user.
	if n := countNotifications(ctx, t, relChatB); n != 2 {
		t.Fatalf("the other subscriber has %d notifications, want 2", n)
	}

	// Unblocking clears the flag and nothing else: there is nothing to resume.
	if err := users.SetBlocked(ctx, relChatA, false); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if err := testDBPool.QueryRow(ctx, `
		SELECT count(*) FROM subscriptions s JOIN users u ON u.id = s.user_id
		WHERE u.telegram_chat_id = $1 AND u.blocked_at IS NULL`, relChatA).Scan(&subsLeft); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if subsLeft != 0 {
		t.Fatalf("unblocking brought back %d subscriptions", subsLeft)
	}
}

// Blocking a chat we have never seen is not an error: my_chat_member can
// arrive for a user who never pressed /start.
func TestBlockingAnUnknownChatIsANoOp(t *testing.T) {
	ctx, _, _ := testReleases(t)
	if err := storage.NewUsers(testDBPool).SetBlocked(ctx, -999000099, true); err != nil {
		t.Fatalf("block unknown chat: %v", err)
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
