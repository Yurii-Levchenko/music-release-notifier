package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in      string
		command string
		args    string
	}{
		{"/start", "/start", ""},
		{"/start abc123", "/start", "abc123"},
		// Telegram appends @botname when several bots share a chat.
		{"/start@music_release_radar_bot", "/start", ""},
		{"/start@music_release_radar_bot token99", "/start", "token99"},
		{"/START", "/start", ""},
		{"/search   radiohead  ", "/search", "radiohead"},
		// Plain text is a search query, not a command.
		{"radiohead", "", "radiohead"},
		{"", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			command, args := splitCommand(strings.TrimSpace(tc.in))
			if command != tc.command || args != tc.args {
				t.Fatalf("splitCommand(%q) = (%q, %q), want (%q, %q)",
					tc.in, command, args, tc.command, tc.args)
			}
		})
	}
}

// HTML mode needs exactly three characters escaped. Artist and album titles come
// straight from MusicBrainz and routinely contain them; an unescaped & produces
// a 400 "can't parse entities" and the user silently loses the notification.
func TestFormatReleaseEscapesHTML(t *testing.T) {
	rel := notify.Release{
		ArtistName:  "Simon & Garfunkel",
		Title:       "<Untitled> \"Album\"",
		PrimaryType: "Album",
		ReleaseDate: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		InfoURL:     "https://musicbrainz.org/release-group/abc?x=1&y=2",
	}

	out := formatRelease(rel)

	for _, raw := range []string{"Simon & Garfunkel", "<Untitled>", "?x=1&y=2"} {
		if strings.Contains(out, raw) {
			t.Fatalf("unescaped %q present in output:\n%s", raw, out)
		}
	}
	for _, want := range []string{"Simon &amp; Garfunkel", "&lt;Untitled&gt;", "x=1&amp;y=2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected escaped %q in output:\n%s", want, out)
		}
	}
	// Our own markup must survive escaping.
	if !strings.Contains(out, "<b>") || !strings.Contains(out, `<a href="`) {
		t.Fatalf("intended markup was escaped away:\n%s", out)
	}
	if !strings.Contains(out, "20.08.2026") {
		t.Fatalf("release date missing:\n%s", out)
	}
}

func TestFormatReleaseWithoutURL(t *testing.T) {
	out := formatRelease(notify.Release{
		ArtistName:  "Aphex Twin",
		Title:       "Selected Ambient Works",
		PrimaryType: "EP",
	})
	if strings.Contains(out, "<a href") {
		t.Fatalf("link rendered without an URL:\n%s", out)
	}
	if !strings.Contains(out, "EP") {
		t.Fatalf("release type missing:\n%s", out)
	}
	// A zero date must not print 01.01.0001.
	if strings.Contains(out, "0001") {
		t.Fatalf("zero date rendered:\n%s", out)
	}
}

func TestParseChatID(t *testing.T) {
	if id, err := parseChatID(" 123456789 "); err != nil || id != 123456789 {
		t.Fatalf("parseChatID = (%d, %v), want (123456789, nil)", id, err)
	}
	// A bot cannot address an @username (SPEC C1), so this must be an error
	// rather than something we try to send to.
	if _, err := parseChatID("@someuser"); err == nil {
		t.Fatal("parseChatID(@someuser) should fail: usernames are not addressable")
	}
	if _, err := parseChatID(""); err == nil {
		t.Fatal("parseChatID(empty) should fail")
	}
}

// The notification used to link only to MusicBrainz, a metadata site. Somebody
// just told their artist released an album wants to press play.
func TestNotificationCarriesListenLinks(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "Drake", Title: "For All The Dogs", PrimaryType: "Album",
		ReleaseDate: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
		Listen: notify.ListenLinks{
			Spotify:    "https://open.spotify.com/artist/3TVXtAsR1Inumwj472S9r4",
			YouTube:    "https://www.youtube.com/channel/UCByOQJjav0CUDwxCk-jVNRQ",
			AppleMusic: "https://music.apple.com/ca/artist/271256",
		},
	})

	for _, want := range []string{"Spotify", "YouTube", "Apple Music", "open.spotify.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("message is missing %q:\n%s", want, got)
		}
	}
	// Spotify first: this project exists because Spotify does not reliably
	// tell you about releases, so that is where most readers will go.
	if strings.Index(got, "Spotify") > strings.Index(got, "YouTube") {
		t.Errorf("YouTube came before Spotify:\n%s", got)
	}
}

// An artist subscribed to before link lookups existed has none, and some
// artists simply have none. That is normal operation, not an edge case.
func TestNotificationWithoutLinksHasNoEmptyRow(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "あいみょん", Title: "Sleepy", PrimaryType: "Single",
	})

	if strings.Contains(got, "▶") {
		t.Fatalf("an empty listen row was rendered:\n%s", got)
	}
}

// Only the services actually present may appear.
func TestOnlyKnownLinksAreRendered(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "X", Title: "Y",
		Listen: notify.ListenLinks{YouTube: "https://www.youtube.com/channel/Z"},
	})

	if !strings.Contains(got, "YouTube") {
		t.Fatalf("YouTube missing:\n%s", got)
	}
	if strings.Contains(got, "Spotify") || strings.Contains(got, "Apple") {
		t.Fatalf("a service with no link was rendered:\n%s", got)
	}
}

// A photo caption is capped at 1024 characters and MusicBrainz titles are not
// capped at all. Exceeding it costs the artwork; exceeding the message limit
// would cost the notification.
func TestAbsurdlyLongTitleIsTrimmed(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "X", Title: strings.Repeat("а", 5000), PrimaryType: "Album",
	})

	if len([]rune(got)) > 1024 {
		t.Fatalf("message is %d runes, over the photo caption limit", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Fatal("the title was cut without saying so")
	}
}

// Counting bytes instead of runes would cut a Japanese title to a third of its
// length and could split a character in half.
func TestTrimCountsRunesNotBytes(t *testing.T) {
	title := strings.Repeat("唇", 100) // 300 bytes, 100 runes

	got := formatRelease(notify.Release{ArtistName: "X", Title: title})

	if !strings.Contains(got, title) {
		t.Fatalf("a 100-rune title was trimmed as if it were 300 characters:\n%s", got)
	}
}
