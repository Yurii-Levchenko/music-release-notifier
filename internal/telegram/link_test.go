package telegram

import "testing"

func TestClassifyLink(t *testing.T) {
	cases := []struct {
		name          string
		query         string
		link, spotify bool
	}{
		// The one that actually happened, straight out of the search cache.
		{
			name:    "the real pasted track url",
			query:   "https://open.spotify.com/track/32hgy3msntiaptunwmvkd0?si=cfdda8de3fe345da",
			link:    true,
			spotify: true,
		},
		{name: "spotify artist url", query: "https://open.spotify.com/artist/73VoAnPGod8PX4FTfoZ8Yl", link: true, spotify: true},
		{name: "spotify short link", query: "https://spotify.link/abc123", link: true, spotify: true},
		{name: "apex domain", query: "http://spotify.com", link: true, spotify: true},
		{name: "uppercase scheme and host", query: "HTTPS://OPEN.SPOTIFY.COM/track/x", link: true, spotify: true},
		{name: "leading www", query: "www.youtube.com/watch?v=x", link: true, spotify: false},
		{name: "some other link", query: "https://music.apple.com/artist/x", link: true, spotify: false},

		// A host that merely ends in the letters, without the dot boundary.
		{name: "lookalike host is not spotify", query: "https://notspotify.com/x", link: true, spotify: false},
		// The word in the path says nothing about where the link points.
		{name: "spotify only in the path", query: "https://example.com/spotify.com/track", link: true, spotify: false},

		// Names must keep working. Refusing to search a real artist would be a
		// worse failure than the one this guard fixes.
		{name: "plain name", query: "radiohead", link: false},
		{name: "name with dots", query: "C.O.M.", link: false},
		{name: "name with a dot-com shape", query: "Lastfm.com", link: false},
		{name: "cyrillic name", query: "Іво Бобул", link: false},
		{name: "japanese name", query: "あいみょん", link: false},
		{name: "name containing http", query: "http goes here", link: false},
		{name: "empty", query: "", link: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, spotify := classifyLink(tc.query)
			if link != tc.link {
				t.Errorf("link = %v, want %v for %q", link, tc.link, tc.query)
			}
			if spotify != tc.spotify {
				t.Errorf("spotify = %v, want %v for %q", spotify, tc.spotify, tc.query)
			}
		})
	}
}
