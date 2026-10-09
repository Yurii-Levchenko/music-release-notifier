package telegram

import (
	"strings"
	"testing"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

func page(names ...string) []storage.Subscription {
	items := make([]storage.Subscription, len(names))
	for i, n := range names {
		items[i] = storage.Subscription{Name: n, MBID: "mbid-" + n}
	}
	return items
}

// The whole point of the encoding: a selection survives a round trip through
// callback_data, so no selection state has to live on the server.
func TestSelectionRoundTrip(t *testing.T) {
	items := page("A", "B", "C", "D")
	s := newSelection(2, items).toggle(0).toggle(3)

	data := s.encode("pick", 1)
	got, index, ok := decodeSelection(strings.TrimPrefix(data, "pick:"))
	if !ok {
		t.Fatalf("could not decode %q", data)
	}
	if index != 1 {
		t.Errorf("index = %d, want 1", index)
	}
	if got.page != s.page || got.mask != s.mask || got.digest != s.digest {
		t.Fatalf("decoded %+v, want %+v", got, s)
	}
}

// callback_data is capped at 64 bytes (C6), and that cap is the reason this
// design is possible at all — so it has to hold for a full page.
func TestEncodedSelectionFitsInCallbackData(t *testing.T) {
	items := make([]storage.Subscription, listPageSize)
	for i := range items {
		items[i] = storage.Subscription{MBID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
	}

	data := selectAll(99, items).encode("pick", listPageSize-1)
	if n := len(data); n > 64 {
		t.Fatalf("callback_data is %d bytes for a full page: %q", n, data)
	}
}

func TestToggleAndCount(t *testing.T) {
	s := newSelection(0, page("A", "B", "C"))

	if s.count() != 0 {
		t.Fatalf("a new selection has %d ticks", s.count())
	}
	s = s.toggle(1)
	if !s.selected(1) || s.selected(0) || s.count() != 1 {
		t.Fatalf("after one toggle: %+v", s)
	}
	s = s.toggle(1)
	if s.selected(1) || s.count() != 0 {
		t.Fatalf("toggling twice did not clear it: %+v", s)
	}
}

func TestSelectAllTicksEveryRow(t *testing.T) {
	items := page("A", "B", "C")
	s := selectAll(0, items)

	if s.count() != 3 {
		t.Fatalf("count = %d, want 3", s.count())
	}
	// And nothing beyond the page: a mask with bits past the last row would
	// resolve to nothing but makes the count lie.
	if s.selected(3) {
		t.Fatal("a row past the end of the page is ticked")
	}
}

// The mask addresses positions, not artists. If the page shifted between the
// tap that drew it and the tap that confirms, those positions now mean
// different people — and this deletes subscriptions.
func TestStalePageIsRefused(t *testing.T) {
	drawn := page("A", "B", "C")
	s := selectAll(0, drawn)

	// Somebody unsubscribed from A on another device.
	now := page("B", "C")

	if _, _, ok := s.mbids(now); ok {
		t.Fatal("a selection was applied to a page that had changed underneath it")
	}
}

func TestSelectionResolvesToTheRightArtists(t *testing.T) {
	items := page("A", "B", "C", "D")
	s := newSelection(0, items).toggle(1).toggle(3)

	mbids, names, ok := s.mbids(items)
	if !ok {
		t.Fatal("a selection was refused against the page it was drawn from")
	}
	if len(mbids) != 2 || mbids[0] != "mbid-B" || mbids[1] != "mbid-D" {
		t.Fatalf("mbids = %v", mbids)
	}
	if names[0] != "B" || names[1] != "D" {
		t.Fatalf("names = %v", names)
	}
}

// Order matters to the digest: two pages holding the same artists in a
// different order address different positions.
func TestDigestNoticesReordering(t *testing.T) {
	if pageDigest(page("A", "B")) == pageDigest(page("B", "A")) {
		t.Fatal("the digest ignores order, so a reordered page would pass as unchanged")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",
		"1",
		"1:ff",
		"1:ff:2",           // no digest
		"1:ff:2:",          // empty digest
		"x:ff:2:abcd",      // page not a number
		"1:zz:2:abcd",      // mask not hex
		"1:ff:x:abcd",      // index not a number
		"-1:ff:2:abcd",     // negative page
		"1:ff:2:abcd:junk", // too many parts
	} {
		if _, _, ok := decodeSelection(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The page inside a pick: callback is multiplied into an OFFSET too, so it gets
// the same upper bound as list: — a forged value past it is malformed, not a
// page (review 16.09, Security #2).
func TestDecodeSelectionRejectsAnAbsurdPage(t *testing.T) {
	if _, _, ok := decodeSelection("100000000:0:0:abcd"); ok {
		t.Fatal("a page far past any real list decoded as valid")
	}
	if _, _, ok := decodeSelection("3:0:0:abcd"); !ok {
		t.Fatal("an ordinary page was rejected")
	}
}
