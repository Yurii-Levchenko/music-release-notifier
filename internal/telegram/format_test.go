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
