package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Every button added in S3 has to fit Telegram's 64-byte callback_data cap
// (SPEC.md C6). The unsubscribe button carries a full 36-character MBID, which
// is the tightest of them, so assert it rather than assume.
func TestSubscriptionCallbackDataFitsLimit(t *testing.T) {
	hash := strings.Repeat("f", storage.QueryHashLen)
	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"

	cases := map[string]string{
		"subscribe":      cbSubscribe + ":" + hash + ":9999",
		"unsub via card": cbUnsubscribe + ":" + hash + ":9999",
		"unsub via list": cbUnsubscribe + ":" + mbid,
		"list page":      cbList + ":9999",
		"stop yes":       cbStop + ":yes",
		"stop no":        cbStop + ":no",
	}
	for name, data := range cases {
		if n := len([]byte(data)); n > 64 {
			t.Fatalf("%s callback data %q is %d bytes, over the 64-byte limit", name, data, n)
		}
	}

	// And through the real renderers, since those are what actually ship.
	_, markup := renderCandidate("q", []musicbrainz.Artist{{MBID: mbid, Name: "X"}}, 0, hash, false, testClock)
	assertCallbackBudget(t, markup.InlineKeyboard)

	items := make([]storage.Subscription, listPageSize)
	for i := range items {
		items[i] = storage.Subscription{MBID: mbid, Name: fmt.Sprintf("Artist %d", i)}
	}
	_, listMarkup := renderList(items, 100, 3, newSelection(3, items), "")
	assertCallbackBudget(t, listMarkup.InlineKeyboard)
}

func assertCallbackBudget(t *testing.T, rows [][]telego.InlineKeyboardButton) {
	t.Helper()
	for _, row := range rows {
		// Index rather than range-copy: an InlineKeyboardButton is 144 bytes.
		for i := range row {
			if n := len([]byte(row[i].CallbackData)); n > 64 {
				t.Fatalf("button %q carries %d bytes of callback data", row[i].Text, n)
			}
		}
	}
}

// Through renderCandidate, not candidateKeyboard.
//
// This test exists because of a real miss: an earlier version asserted the
// keyboard builder in isolation and passed while renderCandidate was still
// calling the old navigation-only builder and ignoring its subscribed
// argument. The linter caught the unused parameter; the test did not. Assert
// the function the bot actually calls.
func TestRenderCandidateRendersTheActionButton(t *testing.T) {
	candidates := []musicbrainz.Artist{
		{MBID: "a74b1b7f-71a5-4011-9441-d0b5e4122711", Name: "Radiohead"},
	}

	for _, tc := range []struct {
		subscribed bool
		wantPrefix string
		wantText   string
	}{
		{false, cbSubscribe + ":", "Підписатись"},
		{true, cbUnsubscribe + ":", "Відписатись"},
	} {
		text, markup := renderCandidate("radiohead", candidates, 0, "abcdef123456", tc.subscribed, testClock)

		var found bool
		for _, row := range markup.InlineKeyboard {
			for i := range row {
				if strings.HasPrefix(row[i].CallbackData, tc.wantPrefix) {
					found = true
					if !strings.Contains(row[i].Text, tc.wantText) {
						t.Fatalf("subscribed=%v: button says %q, want it to mention %q",
							tc.subscribed, row[i].Text, tc.wantText)
					}
				}
			}
		}
		if !found {
			t.Fatalf("subscribed=%v: renderCandidate produced no %q button", tc.subscribed, tc.wantPrefix)
		}

		// The card body should say so too, so state is visible without reading
		// the button.
		if tc.subscribed && !strings.Contains(text, "підписаний") {
			t.Fatalf("subscribed card does not say so:\n%s", text)
		}
		if !tc.subscribed && strings.Contains(text, "Ти підписаний") {
			t.Fatalf("unsubscribed card claims a subscription:\n%s", text)
		}
	}
}

// The card's action button must say what pressing it does, and its payload must
// route to the matching handler. A label that disagrees with the callback is
// the kind of bug that unsubscribes people who meant to subscribe.
func TestCandidateKeyboardActionFollowsState(t *testing.T) {
	hash := "abcdef123456"

	for _, tc := range []struct {
		subscribed bool
		wantPrefix string
	}{
		{false, cbSubscribe},
		{true, cbUnsubscribe},
	} {
		markup := candidateKeyboard(hash, 2, 5, tc.subscribed)

		var action string
		for _, row := range markup.InlineKeyboard {
			for _, b := range row {
				if strings.HasPrefix(b.CallbackData, tc.wantPrefix+":") {
					action = b.CallbackData
				}
			}
		}
		if action == "" {
			t.Fatalf("subscribed=%v: no button with prefix %q", tc.subscribed, tc.wantPrefix)
		}
		gotHash, gotIndex, ok := parseCardTarget(strings.TrimPrefix(action, tc.wantPrefix+":"))
		if !ok || gotHash != hash || gotIndex != 2 {
			t.Fatalf("action payload %q does not round-trip", action)
		}
		// Navigation must still be there; subscribing should not trap the user
		// on one candidate.
		if len(markup.InlineKeyboard) < 2 {
			t.Fatalf("subscribed=%v: navigation row disappeared", tc.subscribed)
		}
	}
}

// The dispatcher tells a card button from a list button by shape alone, so that
// shape check has to be exact — a false positive routes an unsubscribe at the
// wrong handler.
func TestLooksLikeMBID(t *testing.T) {
	valid := []string{
		"a74b1b7f-71a5-4011-9441-d0b5e4122711",
		"AAAAAAAA-0000-4000-8000-000000000001",
	}
	for _, s := range valid {
		if !looksLikeMBID(s) {
			t.Fatalf("looksLikeMBID(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",
		"abcdef123456:2",                        // a card target
		"a74b1b7f-71a5-4011-9441-d0b5e412271",   // one short
		"a74b1b7f-71a5-4011-9441-d0b5e41227111", // one long
		"a74b1b7f71a5-4011-9441-d0b5e4122711",   // dash in the wrong place
		"g74b1b7f-71a5-4011-9441-d0b5e4122711",  // not hex
		"a74b1b7f-71a5-4011-9441-d0b5e412271z",
	}
	for _, s := range invalid {
		if looksLikeMBID(s) {
			t.Fatalf("looksLikeMBID(%q) = true, want false", s)
		}
	}
}

// Numbering has to continue across pages, or the number under an artist on page
// two points at someone on page one.
func TestRenderListNumbersContinueAcrossPages(t *testing.T) {
	items := []storage.Subscription{
		{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Artist A"},
		{MBID: "bbbbbbbb-0000-4000-8000-000000000002", Name: "Artist B"},
	}

	text, markup := renderList(items, 42, 1, newSelection(1, items), "")

	// Page 1 (zero-based) starts at 21.
	if !strings.Contains(text, "21. Artist A") || !strings.Contains(text, "22. Artist B") {
		t.Fatalf("numbering did not continue onto page 2:\n%s", text)
	}
	if !strings.Contains(text, "Сторінка 2 з 3") {
		t.Fatalf("page indicator missing or wrong:\n%s", text)
	}

	// The buttons must be labeled with the same numbers as the lines.
	var labels []string
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if _, err := strconv.Atoi(b.Text); err == nil {
				labels = append(labels, b.Text)
			}
		}
	}
	if len(labels) != 2 || labels[0] != "21" || labels[1] != "22" {
		t.Fatalf("button labels = %v, want [21 22]", labels)
	}

	// And each must address its own row, on this page. The buttons no longer
	// carry an MBID — they carry the selection plus the row to flip, so that a
	// tap selects rather than deletes.
	var rows []int
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if !strings.HasPrefix(btn.CallbackData, cbPick+":") {
				continue
			}
			sel, index, ok := decodeSelection(strings.TrimPrefix(btn.CallbackData, cbPick+":"))
			if !ok {
				t.Fatalf("undecodable button payload %q", btn.CallbackData)
			}
			if sel.page != 1 {
				t.Errorf("button for row %d carries page %d", index, sel.page)
			}
			rows = append(rows, index)
		}
	}
	if len(rows) != 2 || rows[0] != 0 || rows[1] != 1 {
		t.Fatalf("button rows = %v, want [0 1] — positions within the page", rows)
	}
}

// Numbers select; nothing is removed until a second, deliberate press. They
// used to unsubscribe on the first tap, which made a mis-tap irreversible and
// bulk tidying impossible at the same time.
func TestListNumbersSelectRatherThanDelete(t *testing.T) {
	items := []storage.Subscription{
		{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Artist A"},
		{MBID: "bbbbbbbb-0000-4000-8000-000000000002", Name: "Artist B"},
	}

	_, markup := renderList(items, 2, 0, newSelection(0, items), "")

	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, cbUnsubscribe+":") {
				t.Fatalf("a number still deletes on the first tap: %q", btn.CallbackData)
			}
		}
	}
}

// With something ticked, the confirm button appears and says how many.
func TestSelectedRowsGetAConfirmButton(t *testing.T) {
	items := []storage.Subscription{
		{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Artist A"},
		{MBID: "bbbbbbbb-0000-4000-8000-000000000002", Name: "Artist B"},
		{MBID: "cccccccc-0000-4000-8000-000000000003", Name: "Artist C"},
	}
	sel := newSelection(0, items).toggle(0).toggle(2)

	text, markup := renderList(items, 3, 0, sel, "")

	if !strings.Contains(text, "Обрано: 2") {
		t.Errorf("the count is missing from the text:\n%s", text)
	}
	if !strings.Contains(text, "✓ 1. Artist A") || !strings.Contains(text, "✓ 3. Artist C") {
		t.Errorf("ticked rows are not marked:\n%s", text)
	}
	if strings.Contains(text, "✓ 2. Artist B") {
		t.Errorf("an unticked row is marked:\n%s", text)
	}

	var apply string
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if strings.HasPrefix(btn.CallbackData, cbPickApply+":") {
				apply = btn.Text
			}
		}
	}
	if !strings.Contains(apply, "(2)") {
		t.Fatalf("confirm button = %q, want the count", apply)
	}
}

// Nothing ticked: offer to tick the page instead of a confirm button with
// nothing to confirm.
func TestEmptySelectionOffersSelectAll(t *testing.T) {
	items := []storage.Subscription{
		{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "A"},
		{MBID: "bbbbbbbb-0000-4000-8000-000000000002", Name: "B"},
	}

	_, markup := renderList(items, 2, 0, newSelection(0, items), "")

	var prefixes []string
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			prefix, _, _ := strings.Cut(btn.CallbackData, ":")
			prefixes = append(prefixes, prefix)
		}
	}
	joined := strings.Join(prefixes, " ")
	if !strings.Contains(joined, cbPickAll) {
		t.Errorf("no select-all button: %s", joined)
	}
	if strings.Contains(joined, cbPickApply) {
		t.Errorf("a confirm button with nothing selected: %s", joined)
	}
}

// The receipt names what went, so a mis-tap is visible rather than counted.
func TestUnsubscribeReceipt(t *testing.T) {
	if got := unsubscribeReceipt(nil); got != "" {
		t.Errorf("receipt for nothing = %q", got)
	}

	got := unsubscribeReceipt([]string{"Simon & Garfunkel", "Drake"})
	if !strings.Contains(got, "Simon &amp; Garfunkel") {
		t.Errorf("name not escaped: %q", got)
	}
	if !strings.Contains(got, "Drake") {
		t.Errorf("name missing: %q", got)
	}

	many := make([]string, 9)
	for i := range many {
		many[i] = fmt.Sprintf("Artist %d", i)
	}
	long := unsubscribeReceipt(many)
	if !strings.HasSuffix(long, "…") {
		t.Errorf("a truncated receipt does not say so: %q", long)
	}
	if strings.Contains(long, "Artist 8") {
		t.Errorf("the receipt is not bounded: %q", long)
	}
}

// One page means no navigation row; showing dead arrows on a short list is
// noise.
func TestRenderListHidesPagerForOnePage(t *testing.T) {
	items := []storage.Subscription{{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Only"}}
	text, markup := renderList(items, 1, 0, newSelection(0, items), "")

	if strings.Contains(text, "Сторінка") {
		t.Fatalf("page indicator shown for a single page:\n%s", text)
	}
	for _, row := range markup.InlineKeyboard {
		for _, b := range row {
			if b.Text == "◀" || b.Text == "▶" {
				t.Fatalf("navigation arrow rendered for a single page")
			}
		}
	}
}

// Artist names come from MusicBrainz and reach the list verbatim.
func TestRenderListEscapesHTML(t *testing.T) {
	items := []storage.Subscription{
		{MBID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "Simon & Garfunkel <duo>"},
	}
	text, _ := renderList(items, 1, 0, newSelection(0, items), "")

	if strings.Contains(text, "Simon & Garfunkel <duo>") {
		t.Fatalf("unescaped name in list:\n%s", text)
	}
	if !strings.Contains(text, "Simon &amp; Garfunkel &lt;duo&gt;") {
		t.Fatalf("name not escaped as expected:\n%s", text)
	}
}

// A full page of long names must not blow Telegram's 4096-character message cap.
func TestRenderListStaysUnderMessageLimit(t *testing.T) {
	items := make([]storage.Subscription, listPageSize)
	long := strings.Repeat("Very Long Artist Name ", 6)
	for i := range items {
		items[i] = storage.Subscription{
			MBID: "aaaaaaaa-0000-4000-8000-00000000000" + strconv.Itoa(i%10),
			Name: long,
		}
	}
	text, _ := renderList(items, 500, 9, newSelection(9, items), "")
	if n := len([]rune(text)); n > 4096 {
		t.Fatalf("list page is %d characters, over Telegram's 4096 limit", n)
	}
}

// /stop sits in Telegram's command menu, so a mis-tap there used to be two taps
// from destroying every subscription. Nothing but the word deletes anything.
func TestOnlyTheWordConfirmsDeletion(t *testing.T) {
	confirms := []string{"DELETE", "delete", "Delete", "  DELETE  ", "DELETE\n"}
	for _, args := range confirms {
		if !isStopConfirmation(args) {
			t.Errorf("%q did not confirm; somebody who meant it would try three times", args)
		}
	}

	refuses := []string{
		"",           // a bare /stop
		"yes",        // the old button's answer
		"так",        //
		"DELETE ALL", // close, but not the word
		"delet",      //
		"видалити",   //
	}
	for _, args := range refuses {
		if isStopConfirmation(args) {
			t.Errorf("%q deleted everything", args)
		}
	}
}

// The farewell list is the only copy of the data once Forget runs, so it has to
// be complete and correctly escaped.
func TestFarewellListIsCompleteAndEscaped(t *testing.T) {
	items := []storage.Subscription{
		{Name: "Simon & Garfunkel"},
		{Name: "あいміみょん"},
		{Name: "<script>"},
	}

	got := farewellList(items, len(items))

	for i, item := range items {
		if !strings.Contains(got, fmt.Sprintf("%d. ", i+1)) {
			t.Errorf("position %d is missing:\n%s", i+1, got)
		}
		_ = item
	}
	if !strings.Contains(got, "Simon &amp; Garfunkel") {
		t.Errorf("an ampersand reached the message unescaped:\n%s", got)
	}
	if strings.Contains(got, "<script>") {
		t.Errorf("markup reached the message unescaped:\n%s", got)
	}
	if !strings.Contains(got, "あいміみょん") {
		t.Errorf("a non-Latin name was lost:\n%s", got)
	}
}

// A truncated list must say so. Handing somebody an incomplete list that looks
// complete is worse than handing them nothing.
func TestFarewellListAdmitsTruncation(t *testing.T) {
	items := make([]storage.Subscription, 3)
	for i := range items {
		items[i] = storage.Subscription{Name: fmt.Sprintf("Artist %d", i+1)}
	}

	got := farewellList(items, 250)

	if !strings.Contains(got, "250") {
		t.Errorf("the real total is missing:\n%s", got)
	}
	if !strings.Contains(got, "247") {
		t.Errorf("the remainder is not mentioned:\n%s", got)
	}
}

// No truncation notice when nothing was truncated.
func TestFarewellListSaysNothingExtraWhenComplete(t *testing.T) {
	got := farewellList([]storage.Subscription{{Name: "Only One"}}, 1)

	if strings.Contains(got, "та ще") {
		t.Errorf("a complete list claims to be truncated:\n%s", got)
	}
}
