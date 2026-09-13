package storage_test

import (
	"context"
	"testing"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Distinctive so a stray row is obviously a test artifact, and far from any real
// Telegram chat id.
const testChatID int64 = -999000001

func testSubs(t *testing.T) (context.Context, *storage.Subscriptions) {
	t.Helper()
	if testDBPool == nil {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}
	ctx := context.Background()

	// Scoped cleanup: delete only this test's user. The CASCADE takes its
	// subscriptions with it, and nothing else is touched.
	cleanup := func() {
		_, _ = testDBPool.Exec(ctx, `DELETE FROM users WHERE telegram_chat_id = $1`, testChatID)
		_, _ = testDBPool.Exec(ctx,
			`DELETE FROM artists WHERE mbid IN ($1, $2, $3)`, mbidA, mbidB, mbidC)
	}
	cleanup()
	t.Cleanup(cleanup)

	return ctx, storage.NewSubscriptions(testDBPool)
}

const (
	mbidA = "aaaaaaaa-0000-4000-8000-000000000001"
	mbidB = "bbbbbbbb-0000-4000-8000-000000000002"
	mbidC = "cccccccc-0000-4000-8000-000000000003"
)

// NFR-2 through the real API: the same pair from both sources collapses to one
// row, and the caller can tell the first time from the rest.
func TestSubscribeIsIdempotent(t *testing.T) {
	ctx, subs := testSubs(t)
	artist := storage.ArtistRef{MBID: mbidA, Name: "Radiohead"}

	created, err := subs.Subscribe(ctx, testChatID, artist, "bot")
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	if !created {
		t.Fatal("first subscribe reported created=false")
	}

	// Same pair from the other source — the extension in v2 — must not duplicate.
	created, err = subs.Subscribe(ctx, testChatID, artist, "extension")
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	if created {
		t.Fatal("second subscribe reported created=true; the pair must be unique")
	}

	_, total, err := subs.List(ctx, testChatID, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

// Subscribing creates the user if they are not there. A button press can arrive
// after /stop deleted the row, and that must work rather than silently do
// nothing.
func TestSubscribeCreatesMissingUser(t *testing.T) {
	ctx, subs := testSubs(t)

	var before bool
	if err := testDBPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE telegram_chat_id = $1)`, testChatID).
		Scan(&before); err != nil {
		t.Fatalf("check user: %v", err)
	}
	if before {
		t.Fatal("fixture user already exists; cleanup did not run")
	}

	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidA, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	var after bool
	if err := testDBPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE telegram_chat_id = $1)`, testChatID).
		Scan(&after); err != nil {
		t.Fatalf("check user: %v", err)
	}
	if !after {
		t.Fatal("subscribe did not create the user")
	}
}

// The artist name is refreshed on re-subscribe: MusicBrainz edits happen, and a
// stale name in someone's list is a small but real wrong.
func TestSubscribeRefreshesArtistName(t *testing.T) {
	ctx, subs := testSubs(t)

	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidA, Name: "Old Name"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidA, Name: "New Name"}, "bot"); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}

	items, _, err := subs.List(ctx, testChatID, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Name != "New Name" {
		t.Fatalf("items = %+v, want the refreshed name", items)
	}
}

func TestSubscribeRejectsEmptyMBID(t *testing.T) {
	ctx, subs := testSubs(t)
	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: "", Name: "Nobody"}, "bot"); err == nil {
		t.Fatal("subscribe accepted an empty MBID")
	}
}

func TestUnsubscribe(t *testing.T) {
	ctx, subs := testSubs(t)
	artist := storage.ArtistRef{MBID: mbidA, Name: "Radiohead"}

	if _, err := subs.Subscribe(ctx, testChatID, artist, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	removed, err := subs.Unsubscribe(ctx, testChatID, mbidA)
	if err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if !removed {
		t.Fatal("unsubscribe reported removed=false for an existing subscription")
	}

	// Unsubscribing twice is not an error — a double tap, or a stale button.
	removed, err = subs.Unsubscribe(ctx, testChatID, mbidA)
	if err != nil {
		t.Fatalf("second unsubscribe: %v", err)
	}
	if removed {
		t.Fatal("second unsubscribe reported removed=true")
	}

	// An MBID that was never subscribed is the same non-event.
	removed, err = subs.Unsubscribe(ctx, testChatID, mbidC)
	if err != nil || removed {
		t.Fatalf("unsubscribe from an unknown artist: removed=%v err=%v", removed, err)
	}
}

func TestIsSubscribed(t *testing.T) {
	ctx, subs := testSubs(t)

	// Before anything exists — including the user — this must answer false, not
	// error. The card renders for people who have never pressed /start.
	yes, err := subs.IsSubscribed(ctx, testChatID, mbidA)
	if err != nil || yes {
		t.Fatalf("IsSubscribed before any user: %v err=%v", yes, err)
	}

	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidA, Name: "Radiohead"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if yes, err = subs.IsSubscribed(ctx, testChatID, mbidA); err != nil || !yes {
		t.Fatalf("IsSubscribed after subscribe: %v err=%v", yes, err)
	}
	if yes, err = subs.IsSubscribed(ctx, testChatID, mbidB); err != nil || yes {
		t.Fatalf("IsSubscribed for a different artist: %v err=%v", yes, err)
	}
}

// Paging must be stable: sorted by subscription time, oldest first, so a new
// subscription appends rather than shifting everything already on a page.
func TestListPagination(t *testing.T) {
	ctx, subs := testSubs(t)

	// A slice, not a map: the order these are created in is exactly what the
	// list is now sorted by, and ranging a map would make the expectation
	// depend on Go's hash seed. The previous version of this test asserted
	// alphabetical order, which hid that.
	for _, sub := range []struct{ mbid, name string }{
		{mbidA, "Charlie"}, // subscribed first, so listed first
		{mbidB, "Alice"},
		{mbidC, "Bob"}, // subscribed last, so listed last
	} {
		if _, err := subs.Subscribe(ctx, testChatID,
			storage.ArtistRef{MBID: sub.mbid, Name: sub.name}, "bot"); err != nil {
			t.Fatalf("subscribe %s: %v", sub.name, err)
		}
	}

	page1, total, err := subs.List(ctx, testChatID, 2, 0)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	// Oldest first, so a number keeps meaning the same artist. The buttons
	// unsubscribe by position, and a list that renumbers itself on every new
	// subscription is a way to unsubscribe from the wrong artist.
	if len(page1) != 2 || page1[0].Name != "Charlie" || page1[1].Name != "Alice" {
		t.Fatalf("page 1 = %+v, want Charlie then Alice", page1)
	}
	if page1[0].SubscribedAt.After(page1[1].SubscribedAt) {
		t.Fatalf("page 1 is not in ascending subscription order: %+v", page1)
	}
	if page1[0].SubscribedAt.IsZero() {
		t.Fatal("SubscribedAt was not returned")
	}

	page2, _, err := subs.List(ctx, testChatID, 2, 2)
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2) != 1 || page2[0].Name != "Bob" {
		t.Fatalf("page 2 = %+v, want Bob", page2)
	}

	// Past the end is empty, not an error: the last item can vanish under a
	// user who is looking at the last page.
	past, _, err := subs.List(ctx, testChatID, 2, 99)
	if err != nil || len(past) != 0 {
		t.Fatalf("beyond the end: %d items, err=%v", len(past), err)
	}
}

func TestListForUnknownUser(t *testing.T) {
	ctx, subs := testSubs(t)
	items, total, err := subs.List(ctx, testChatID, 10, 0)
	if err != nil {
		t.Fatalf("list for a user who never started: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Fatalf("total=%d items=%d, want 0", total, len(items))
	}
}

// /stop must actually delete. Telegram's terms require honoring a deletion
// request, and a soft delete would not be one.
func TestForgetCascades(t *testing.T) {
	ctx, subs := testSubs(t)

	for _, mbid := range []string{mbidA, mbidB} {
		if _, err := subs.Subscribe(ctx, testChatID,
			storage.ArtistRef{MBID: mbid, Name: "Someone"}, "bot"); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
	}

	existed, err := subs.Forget(ctx, testChatID)
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !existed {
		t.Fatal("forget reported existed=false for a user with subscriptions")
	}

	var users, subsLeft int
	if err := testDBPool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM users WHERE telegram_chat_id = $1),
		        (SELECT count(*) FROM subscriptions s
		         JOIN users u ON u.id = s.user_id WHERE u.telegram_chat_id = $1)`,
		testChatID).Scan(&users, &subsLeft); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if users != 0 || subsLeft != 0 {
		t.Fatalf("after forget: %d users, %d subscriptions, want 0 and 0", users, subsLeft)
	}

	// The artists themselves survive — they are shared reference data, not the
	// user's property.
	var artists int
	if err := testDBPool.QueryRow(ctx,
		`SELECT count(*) FROM artists WHERE mbid IN ($1, $2)`, mbidA, mbidB).Scan(&artists); err != nil {
		t.Fatalf("count artists: %v", err)
	}
	if artists != 2 {
		t.Fatalf("forget deleted %d shared artist rows; it must only delete the user", 2-artists)
	}

	// Forgetting twice is a non-event.
	existed, err = subs.Forget(ctx, testChatID)
	if err != nil || existed {
		t.Fatalf("second forget: existed=%v err=%v", existed, err)
	}
}

// The property the order exists for: a new subscription must not renumber the
// ones already there. The buttons act on position, so a shifting list is a way
// to unsubscribe from the wrong artist.
func TestSubscribingDoesNotRenumberTheExistingList(t *testing.T) {
	ctx, subs := testSubs(t)

	for _, sub := range []struct{ mbid, name string }{
		{mbidA, "First"}, {mbidB, "Second"},
	} {
		if _, err := subs.Subscribe(ctx, testChatID,
			storage.ArtistRef{MBID: sub.mbid, Name: sub.name}, "bot"); err != nil {
			t.Fatalf("subscribe %s: %v", sub.name, err)
		}
	}

	before, _, err := subs.List(ctx, testChatID, 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidC, Name: "Third"}, "bot"); err != nil {
		t.Fatalf("subscribe Third: %v", err)
	}

	after, _, err := subs.List(ctx, testChatID, 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(after) != len(before)+1 {
		t.Fatalf("list went from %d to %d entries", len(before), len(after))
	}
	for i := range before {
		if after[i].MBID != before[i].MBID {
			t.Fatalf("position %d changed from %q to %q after a new subscription",
				i+1, before[i].Name, after[i].Name)
		}
	}
	if after[len(after)-1].Name != "Third" {
		t.Fatalf("the new subscription is at position %d, not last", len(after))
	}
}

// Bulk removal is one statement: a loop that failed halfway would leave a list
// the user has to reconcile by hand.
func TestUnsubscribeManyRemovesExactlyTheGivenArtists(t *testing.T) {
	ctx, subs := testSubs(t)

	for _, sub := range []struct{ mbid, name string }{
		{mbidA, "A"}, {mbidB, "B"}, {mbidC, "C"},
	} {
		if _, err := subs.Subscribe(ctx, testChatID,
			storage.ArtistRef{MBID: sub.mbid, Name: sub.name}, "bot"); err != nil {
			t.Fatalf("subscribe %s: %v", sub.name, err)
		}
	}

	removed, err := subs.UnsubscribeMany(ctx, testChatID, []string{mbidA, mbidC})
	if err != nil {
		t.Fatalf("unsubscribe many: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	items, total, err := subs.List(ctx, testChatID, 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || items[0].MBID != mbidB {
		t.Fatalf("left with %+v, want only B", items)
	}
}

// One user's bulk removal must not touch another's subscriptions to the same
// artists.
func TestUnsubscribeManyIsScopedToOneUser(t *testing.T) {
	ctx, subs := testSubs(t)
	const otherChat int64 = testChatID - 1

	for _, chat := range []int64{testChatID, otherChat} {
		if _, err := subs.Subscribe(ctx, chat,
			storage.ArtistRef{MBID: mbidA, Name: "Shared"}, "bot"); err != nil {
			t.Fatalf("subscribe for %d: %v", chat, err)
		}
	}
	t.Cleanup(func() { _, _ = subs.Forget(context.Background(), otherChat) })

	if _, err := subs.UnsubscribeMany(ctx, testChatID, []string{mbidA}); err != nil {
		t.Fatalf("unsubscribe many: %v", err)
	}

	_, total, err := subs.List(ctx, otherChat, 20, 0)
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if total != 1 {
		t.Fatalf("the other user has %d subscriptions left, want 1", total)
	}
}

// An empty selection must not become "delete everything".
func TestUnsubscribeManyWithNothingSelected(t *testing.T) {
	ctx, subs := testSubs(t)

	if _, err := subs.Subscribe(ctx, testChatID,
		storage.ArtistRef{MBID: mbidA, Name: "A"}, "bot"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	removed, err := subs.UnsubscribeMany(ctx, testChatID, nil)
	if err != nil {
		t.Fatalf("unsubscribe many: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d with nothing selected", removed)
	}

	if _, total, _ := subs.List(ctx, testChatID, 20, 0); total != 1 {
		t.Fatalf("subscriptions left = %d, want 1", total)
	}
}
