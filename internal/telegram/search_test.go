package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

func sampleCandidates() []musicbrainz.Artist {
	return []musicbrainz.Artist{
		{
			MBID: "a74b1b7f-71a5-4011-9441-d0b5e4122711", Name: "Radiohead", Score: 100,
			Type: "Group", Country: "GB", Begin: "1991",
			Tags: []string{"alternative rock", "rock"},
		},
		{
			MBID: "b1c2d3e4-0000-0000-0000-000000000002", Name: "On a Friday", Score: 65,
			Type: "Group", Begin: "1985", End: "1991",
			Disambiguation: "pre-Radiohead group, until 1991",
		},
		{MBID: "c1c2d3e4-0000-0000-0000-000000000003", Name: "Radiohead 2", Score: 63},
	}
}

// Telegram rejects callback_data over 64 BYTES (SPEC.md C6), and the failure is
// a 400 on send — after the card is already built. Bytes, not runes: the limit
// is on the encoded payload.
func TestCallbackDataFitsTelegramLimit(t *testing.T) {
	hash := strings.Repeat("f", storage.QueryHashLen)

	// An index far past anything real, to prove the format has headroom.
	for _, index := range []int{0, 9, 99, 9999} {
		data := navData(hash, index)
		if n := len([]byte(data)); n > 64 {
			t.Fatalf("navData(%q, %d) = %q is %d bytes, over Telegram's 64-byte limit",
				hash, index, data, n)
		}
	}
	if n := len([]byte(cbNoop)); n > 64 {
		t.Fatalf("noop callback data is %d bytes", n)
	}

	// And the whole keyboard, since every button carries its own payload.
	markup := navigationKeyboard(hash, 1, 5)
	for _, row := range markup.InlineKeyboard {
		for _, btn := range row {
			if n := len([]byte(btn.CallbackData)); n > 64 {
				t.Fatalf("button %q carries %d bytes of callback data", btn.Text, n)
			}
		}
	}
}

func TestParseCardTarget(t *testing.T) {
	hash, index, ok := parseCardTarget("abcdef123456:3")
	if !ok || hash != "abcdef123456" || index != 3 {
		t.Fatalf("parse = (%q, %d, %v)", hash, index, ok)
	}

	// Callback data comes from the client. An old message from a previous build,
	// or someone poking at the API, must not produce a panic or a wild index.
	for _, bad := range []string{
		"", "abc", "abc:def", ":3", "abc:", "abc:-", "abc:3:4",
	} {
		if _, _, ok := parseCardTarget(bad); ok {
			t.Fatalf("parseCardTarget(%q) reported ok, want rejected", bad)
		}
	}

	// A negative index parses; the handler clamps it. Parsing is not the place
	// to decide policy.
	if _, idx, ok := parseCardTarget("abcdef123456:-2"); !ok || idx != -2 {
		t.Fatalf("negative index should parse, got idx=%d ok=%v", idx, ok)
	}
}

// Arrows that would go nowhere must not be rendered, or the user taps a button
// that answers "nothing happened".
func TestNavigationKeyboardHidesDeadArrows(t *testing.T) {
	hash := "abcdef123456"

	texts := func(index, total int) []string {
		var out []string
		for _, row := range navigationKeyboard(hash, index, total).InlineKeyboard {
			for _, b := range row {
				out = append(out, b.Text)
			}
		}
		return out
	}

	if got, want := texts(0, 3), []string{"1 / 3", "▶"}; !equalStrings(got, want) {
		t.Fatalf("first page buttons = %v, want %v", got, want)
	}
	if got, want := texts(1, 3), []string{"◀", "2 / 3", "▶"}; !equalStrings(got, want) {
		t.Fatalf("middle page buttons = %v, want %v", got, want)
	}
	if got, want := texts(2, 3), []string{"◀", "3 / 3"}; !equalStrings(got, want) {
		t.Fatalf("last page buttons = %v, want %v", got, want)
	}
	// A single result needs no arrows at all.
	if got, want := texts(0, 1), []string{"1 / 1"}; !equalStrings(got, want) {
		t.Fatalf("single result buttons = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Artist names come from MusicBrainz and routinely contain & and <. Unescaped,
// they produce a 400 "can't parse entities" and the user gets nothing.
func TestRenderCandidateEscapesHTML(t *testing.T) {
	candidates := []musicbrainz.Artist{{
		MBID:           "a1b2c3d4-0000-0000-0000-00000000000a",
		Name:           "Simon & Garfunkel <the duo>",
		Type:           "Group",
		Disambiguation: `known as "S&G"`,
		Tags:           []string{"folk & rock"},
	}}

	text, _ := renderCandidate("s&g <query>", candidates, 0, "abcdef123456", false, testClock)

	for _, raw := range []string{"Simon & Garfunkel", "<the duo>", `"S&G"`, "s&g <query>"} {
		if strings.Contains(text, raw) {
			t.Fatalf("unescaped %q present in card:\n%s", raw, text)
		}
	}
	for _, want := range []string{"Simon &amp; Garfunkel", "&lt;the duo&gt;", "folk &amp; rock"} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected escaped %q in card:\n%s", want, text)
		}
	}
	// Our own markup must survive.
	if !strings.Contains(text, "<b>") || !strings.Contains(text, `<a href="https://musicbrainz.org/artist/`) {
		t.Fatalf("intended markup was escaped away:\n%s", text)
	}
}

// Telegram caps a message at 4096 characters. Five candidates with long names,
// disambiguations and tags must not get anywhere near it.
func TestRenderCandidateStaysUnderMessageLimit(t *testing.T) {
	long := strings.Repeat("Very Long Artist Name ", 10)
	candidates := []musicbrainz.Artist{{
		MBID:           "a1b2c3d4-0000-0000-0000-00000000000a",
		Name:           long,
		Type:           "Group",
		Country:        "GB",
		Begin:          "1991",
		End:            "2020",
		Disambiguation: strings.Repeat("disambiguation text ", 20),
		Tags:           []string{long, long, long},
	}}

	text, _ := renderCandidate(long, candidates, 0, "abcdef123456", false, testClock)
	if n := len([]rune(text)); n > 4096 {
		t.Fatalf("card is %d characters, over Telegram's 4096 limit", n)
	}
}

// The facts line is what separates two artists with the same name, so it has to
// degrade gracefully rather than print empty separators.
func TestArtistFacts(t *testing.T) {
	cases := []struct {
		name string
		in   musicbrainz.Artist
		want string
	}{
		{"full", musicbrainz.Artist{Type: "Group", Country: "GB", Begin: "1991"}, "Гурт · GB · з 1991"},
		{"disbanded", musicbrainz.Artist{Type: "Group", Begin: "1985", End: "1991"}, "Гурт · 1985–1991"},
		{"person", musicbrainz.Artist{Type: "Person", Country: "US"}, "Виконавець · US"},
		{"ended only", musicbrainz.Artist{Type: "Person", End: "2016"}, "Виконавець · до 2016"},
		// MusicBrainz leaves type empty for obscure artists; say nothing rather
		// than invent a label.
		{"no type", musicbrainz.Artist{Country: "UA", Begin: "2004"}, "UA · з 2004"},
		{"unknown type passes through", musicbrainz.Artist{Type: "Orchestra"}, "Orchestra"},
		{"nothing known", musicbrainz.Artist{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := artistFacts(tc.in, testClock); got != tc.want {
				t.Fatalf("artistFacts(%+v, testClock) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderCandidateShowsPosition(t *testing.T) {
	candidates := sampleCandidates()

	for i := range candidates {
		_, markup := renderCandidate("radiohead", candidates, i, "abcdef123456", false, testClock)
		var counter string
		for _, row := range markup.InlineKeyboard {
			for _, b := range row {
				if strings.Contains(b.Text, "/") {
					counter = b.Text
				}
			}
		}
		if want := strings.TrimSpace(counter); want == "" {
			t.Fatalf("candidate %d has no position counter", i)
		}
	}
}

// Drake is a Person, so MusicBrainz's life-span begin is his birthday — not
// the start of his career. Rendering it as "з 1986-10-24" claimed he had been
// recording since he was born.
func TestPersonShowsAgeNotACareerStart(t *testing.T) {
	got := artistFacts(musicbrainz.Artist{
		Type: "Person", Country: "CA", Begin: "1986-10-24",
	}, testClock)

	if strings.Contains(got, "з 1986") {
		t.Fatalf("a person's birth year is presented as a career start: %q", got)
	}
	// testClock is November, so the October birthday has passed.
	if !strings.Contains(got, "40 років") {
		t.Fatalf("facts = %q, want an age", got)
	}
	if strings.Contains(got, "10-24") {
		t.Fatalf("the full date of birth is on the card: %q", got)
	}
}

// The birthday not having happened yet this year must take a year off, or
// every artist is a year too old for most of the calendar.
func TestAgeAccountsForTheBirthdayNotYetPassed(t *testing.T) {
	before := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	drake := musicbrainz.Artist{Type: "Person", Begin: "1986-10-24"}

	if got := artistFacts(drake, before); !strings.Contains(got, "39") {
		t.Errorf("before the birthday = %q, want 39", got)
	}
	if got := artistFacts(drake, after); !strings.Contains(got, "40") {
		t.Errorf("after the birthday = %q, want 40", got)
	}
}

// A bare year cannot place a birthday, so an age computed from it is off by
// one for most of the year. Better to show the year than to be confidently
// wrong.
func TestCoarseBirthDateFallsBackToTheYear(t *testing.T) {
	for _, begin := range []string{"1986", "1986-10"} {
		got := artistFacts(musicbrainz.Artist{Type: "Person", Begin: begin}, testClock)
		if !strings.Contains(got, "нар. 1986") {
			t.Errorf("begin %q = %q, want a year fallback rather than a guessed age", begin, got)
		}
	}
}

// Ukrainian agreement has three forms and the teens are the trap: 11 is
// "років", not "рік", even though it ends in 1.
func TestAgeAgreement(t *testing.T) {
	cases := map[int]string{
		1:   "1 рік",
		2:   "2 роки",
		4:   "4 роки",
		5:   "5 років",
		11:  "11 років",
		12:  "12 років",
		14:  "14 років",
		21:  "21 рік",
		22:  "22 роки",
		25:  "25 років",
		31:  "31 рік",
		40:  "40 років",
		101: "101 рік",
		111: "111 років",
	}
	for age, want := range cases {
		if got := ageWords(age); got != want {
			t.Errorf("ageWords(%d) = %q, want %q", age, got, want)
		}
	}
}

// A date MusicBrainz accepted that cannot describe a living person must not
// produce a nonsense age.
func TestImplausibleBirthDateShowsNoAge(t *testing.T) {
	got := artistFacts(musicbrainz.Artist{Type: "Person", Country: "IT", Begin: "1567-03-02"}, testClock)

	if strings.Contains(got, "рок") || strings.Contains(got, "рік") {
		t.Fatalf("a 459-year-old was given an age: %q", got)
	}
	if !strings.Contains(got, "нар. 1567") {
		t.Fatalf("facts = %q, want the year", got)
	}
}

// For a group the same field really is the formation date, so "з" is right.
func TestGroupLifeSpanIsLabeledAsFormation(t *testing.T) {
	got := artistFacts(musicbrainz.Artist{
		Type: "Group", Country: "GB", Begin: "1985",
	}, testClock)

	if !strings.Contains(got, "з 1985") {
		t.Fatalf("facts = %q, want a formation label", got)
	}
}

// An unknown type must not be asserted to be a person. "Formed in" is the
// safer reading when we do not know what the act is.
func TestUnknownTypeDoesNotClaimABirthDate(t *testing.T) {
	got := artistFacts(musicbrainz.Artist{Begin: "1999"}, testClock)

	if strings.Contains(got, "нар.") {
		t.Fatalf("a birth date was asserted about an artist of unknown type: %q", got)
	}
	if !strings.Contains(got, "з 1999") {
		t.Fatalf("facts = %q", got)
	}
}

// A closed range needs no label: it reads correctly as a lifetime for a person
// and as an active period for a group.
func TestClosedRangeNeedsNoLabel(t *testing.T) {
	person := artistFacts(musicbrainz.Artist{Type: "Person", Begin: "1971-06-16", End: "1996-09-13"}, testClock)
	if !strings.Contains(person, "1971–1996") {
		t.Fatalf("facts = %q, want a bare range", person)
	}
	if strings.Contains(person, "нар.") {
		t.Fatalf("a closed range was also labeled as a birth: %q", person)
	}
}

func TestMalformedDatesAreDropped(t *testing.T) {
	for _, bad := range []string{"", "19", "unknown", "circa 1980"} {
		got := artistFacts(musicbrainz.Artist{Type: "Group", Country: "US", Begin: bad}, testClock)
		if strings.Contains(got, bad) && bad != "" {
			t.Errorf("unparseable date %q reached the card: %q", bad, got)
		}
		if !strings.Contains(got, "Гурт") {
			t.Errorf("the rest of the facts were lost for %q: %q", bad, got)
		}
	}
}

// testClock fixes "today" so an age assertion does not change with the
// calendar. Chosen after Drake's October birthday so the arithmetic is
// unambiguous.
var testClock = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
