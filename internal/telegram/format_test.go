package telegram

import (
	"regexp"
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

	out := formatRelease(rel, testClock)

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
	}, testClock)
	// The title must not be a link without an URL to point at. The listen row
	// below it legitimately is one, so this checks the title's own line.
	title := strings.Split(out, nlChar)[2]
	if strings.Contains(title, "<a href") {
		t.Fatalf("the title was linked without an URL:\n%s", out)
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

// A link to the release itself does not exist — MusicBrainz carries streaming
// links for artists only (C46) — so the row links to a search for the release
// on each platform. When the release is there the search lands on it; when it
// is not, the empty result answers the question that three artist-page links
// left open.
func TestListenRowSearchesForTheRelease(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "Ariana Grande", Title: "eternal sunshine", PrimaryType: "Album",
		Links: notify.ArtistLinks{
			Spotify:    "https://open.spotify.com/artist/66CXWjxzNUsdJxJ2JdwvnR",
			YouTube:    "https://www.youtube.com/channel/UC0VOyT2OCBKdQhF3BAbZ-1g",
			AppleMusic: "https://music.apple.com/us/artist/412778295",
		},
	}, testClock)

	// The release title has to be in every query, or the link is no better
	// than the artist page it replaced.
	for _, want := range []string{
		"open.spotify.com/search/",
		"youtube.com/results?search_query=",
		"music.apple.com/search?term=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "open.spotify.com/artist/") {
		t.Errorf("still linking to the artist page:\n%s", got)
	}
	if !strings.Contains(got, "eternal") {
		t.Errorf("the release title is not in the search query:\n%s", got)
	}
	// Spotify first: this project exists because Spotify does not reliably
	// tell you about releases.
	if strings.Index(got, "Spotify") > strings.Index(got, "YouTube") {
		t.Errorf("YouTube came before Spotify:\n%s", got)
	}
}

// Most of the feed is obscure artists. Offering an Apple Music search for an
// artist MusicBrainz says has no Apple presence is the same dead end this
// change removes.
func TestOnlyPlatformsTheArtistIsKnownOnAreOffered(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "LATERNO", Title: "Briefly",
		Links: notify.ArtistLinks{Spotify: "https://open.spotify.com/artist/X"},
	}, testClock)

	if !strings.Contains(got, "Spotify") {
		t.Fatalf("Spotify missing:\n%s", got)
	}
	if strings.Contains(got, "YouTube") || strings.Contains(got, "Apple") {
		t.Fatalf("a platform the artist is not known on was offered:\n%s", got)
	}
}

// Absent data is not evidence of absence: MusicBrainz coverage is patchy, so
// knowing nothing about an artist must not mean offering nothing.
func TestAllPlatformsWhenNothingIsKnown(t *testing.T) {
	got := formatRelease(notify.Release{ArtistName: "Soft Vein", Title: "All We Have Known"}, testClock)

	for _, want := range []string{"Spotify", "YouTube", "Apple Music"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q for an artist with no known links:\n%s", want, got)
		}
	}
}

// An unbounded title must not become an unbounded URL, three times over.
func TestSearchQueryIsBounded(t *testing.T) {
	got := searchQuery("Artist", strings.Repeat("а", 5000))

	if n := len([]rune(got)); n > maxQueryRunes {
		t.Fatalf("query is %d runes, want at most %d", n, maxQueryRunes)
	}
	// No ellipsis: it is a character the search would try to match.
	if strings.Contains(got, "…") {
		t.Fatalf("the query carries an ellipsis: %q", got)
	}
}

// A release with no title has nothing to search for, which is the only case
// where the row is genuinely empty. An artist with no *known* links is not
// that case — see TestAllPlatformsWhenNothingIsKnown.
func TestNoTitleMeansNoListenRow(t *testing.T) {
	got := formatRelease(notify.Release{PrimaryType: "Single"}, testClock)

	if strings.Contains(got, "▶") {
		t.Fatalf("a listen row was rendered with nothing to search for:\n%s", got)
	}
}

// Only the services actually present may appear.
func TestOnlyKnownLinksAreRendered(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "X", Title: "Y",
		Links: notify.ArtistLinks{YouTube: "https://www.youtube.com/channel/Z"},
	}, testClock)

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
	}, testClock)

	// Telegram counts a caption after entity parsing, so URLs inside href
	// attributes do not count — the earlier version of this test measured the
	// raw HTML and only passed by accident, before three search URLs were
	// added to it.
	if n := len([]rune(visibleText(got))); n > 1024 {
		t.Fatalf("caption text is %d characters, over the 1024 limit:\n%s", n, got)
	}
	if !strings.Contains(got, "…") {
		t.Fatal("the title was cut without saying so")
	}
}

// Counting bytes instead of runes would cut a Japanese title to a third of its
// length and could split a character in half.
func TestTrimCountsRunesNotBytes(t *testing.T) {
	title := strings.Repeat("唇", 100) // 300 bytes, 100 runes

	got := formatRelease(notify.Release{ArtistName: "X", Title: title}, testClock)

	if !strings.Contains(got, title) {
		t.Fatalf("a 100-rune title was trimmed as if it were 300 characters:\n%s", got)
	}
}

// The handle goes in the header rather than with the streaming links, because
// it answers a different question: those are "where do I play this", this is
// "who is this".
func TestInstagramHandleIsInTheHeader(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "あいみょん", Title: "Sleepy", PrimaryType: "Single",
		Links: notify.ArtistLinks{
			Instagram: "https://www.instagram.com/aimyon36/",
			Spotify:   "https://open.spotify.com/artist/X",
		},
	}, testClock)

	header := strings.SplitN(got, "\n", 2)[0]
	if !strings.Contains(header, "@aimyon36") {
		t.Fatalf("header = %q, want the handle", header)
	}
	if !strings.Contains(header, "instagram.com/aimyon36") {
		t.Fatalf("the handle is not a link: %q", header)
	}
}

// Knowing only the artist's Instagram says nothing about their streaming
// presence, so all three searches are still offered — and the handle still
// belongs in the header.
func TestInstagramAloneStillOffersSearches(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "X", Title: "Y",
		Links: notify.ArtistLinks{Instagram: "https://www.instagram.com/x/"},
	}, testClock)

	if !strings.Contains(got, "@x") {
		t.Fatalf("the handle is missing:\n%s", got)
	}
	for _, want := range []string{"Spotify", "YouTube", "Apple Music"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// A URL that is not a profile must render nothing rather than "@".
func TestNonProfileInstagramURLRendersNothing(t *testing.T) {
	got := formatRelease(notify.Release{
		ArtistName: "X", Title: "Y",
		Links: notify.ArtistLinks{Instagram: "https://www.instagram.com/p/Cabcdef/"},
	}, testClock)

	if strings.Contains(got, "@") {
		t.Fatalf("a post URL was rendered as a handle:\n%s", got)
	}
}

// nlChar is a newline, named so a test can split on it without embedding one
// in a format string.
const nlChar = "\n"

// visibleText strips HTML tags, leaving what Telegram counts against the
// caption and message limits.
func visibleText(html string) string {
	return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(html, "")
}

// The wording follows how old the release is, not how the notification came to
// be queued. Those are different questions and only the first is the reader's:
// measured on live data, the ordinary fan-out path has announced releases 3, 5
// and 7 days after their release date, every one of them saying "new".
func TestHeadlineFollowsTheReleaseDate(t *testing.T) {
	now := time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		released time.Time
		want     string
	}{
		{"today", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), "новий реліз"},
		{"yesterday", time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC), "новий реліз"},
		{"two days", time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), "недавній реліз від 10 вересня"},
		{"five days", time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC), "недавній реліз від 7 вересня"},
		{"a week", time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC), "недавній реліз від 5 вересня"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headline(tc.released, now); got != tc.want {
				t.Fatalf("headline = %q, want %q", got, tc.want)
			}
		})
	}
}

// Yesterday is the normal healthy path — the poller runs daily — so it must
// not read as late.
func TestYesterdayIsStillNew(t *testing.T) {
	now := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, 9, 11, 22, 0, 0, 0, time.UTC)

	if got := headline(yesterday, now); got != "новий реліз" {
		t.Fatalf("headline = %q, want it to still read as new", got)
	}
}

// A missing or future date must not produce a strange headline. The poller
// guards against future dates; if one slips through, "new" is the least wrong
// thing to call it.
func TestHeadlineHandlesMissingAndFutureDates(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	if got := headline(time.Time{}, now); got != "новий реліз" {
		t.Errorf("missing date = %q", got)
	}
	if got := headline(now.AddDate(0, 0, 3), now); got != "новий реліз" {
		t.Errorf("future date = %q", got)
	}
}

// Ukrainian puts the month in the genitive after a date: "від 7 вересня", not
// "від 7 вересень".
func TestMonthGenitiveForms(t *testing.T) {
	for month, want := range map[time.Month]string{
		time.January: "1 січня", time.March: "3 березня", time.May: "5 травня",
		time.September: "9 вересня", time.November: "11 листопада", time.December: "12 грудня",
	} {
		got := dayAndMonth(time.Date(2026, month, int(month), 0, 0, 0, 0, time.UTC))
		if got != want {
			t.Errorf("dayAndMonth(%v) = %q, want %q", month, got, want)
		}
	}
}

// The header is what shows in a lock-screen preview, so an old release has to
// say so there and not only on the line below.
func TestAnOldReleaseSaysSoInTheHeader(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	got := formatRelease(notify.Release{
		ArtistName: "Ado", Title: "好きでいて", PrimaryType: "Single",
		ReleaseDate: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}, now)

	header := strings.SplitN(got, nlChar, 2)[0]
	if !strings.Contains(header, "від 7 вересня") {
		t.Fatalf("header = %q, want the release date", header)
	}
	if strings.Contains(header, "новий") {
		t.Fatalf("a five-day-old release is announced as new: %q", header)
	}
}
