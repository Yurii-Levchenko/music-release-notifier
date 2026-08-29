package telegram

import (
	"strings"
	"testing"

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

	text, _ := renderCandidate("s&g <query>", candidates, 0, "abcdef123456", false)

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

	text, _ := renderCandidate(long, candidates, 0, "abcdef123456", false)
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
			if got := artistFacts(tc.in); got != tc.want {
				t.Fatalf("artistFacts(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderCandidateShowsPosition(t *testing.T) {
	candidates := sampleCandidates()

	for i := range candidates {
		_, markup := renderCandidate("radiohead", candidates, i, "abcdef123456", false)
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
