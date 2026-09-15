package telegram

import "strings"

// classifyLink reports whether a query is a pasted URL, and whether it points at
// Spotify.
//
// Deliberately narrow: only a scheme or a leading "www.". Matching anything
// that merely looks domain-shaped would start refusing real names — MusicBrainz
// carries artists called "C.O.M.", ".嘘", and plenty of names with dots in them,
// and turning a working search into a lecture about links is a worse failure
// than the one this fixes. A leading scheme cannot be a name typed by accident.
func classifyLink(query string) (link, spotify bool) {
	q := strings.ToLower(strings.TrimSpace(query))

	switch {
	case strings.HasPrefix(q, "http://"), strings.HasPrefix(q, "https://"):
	case strings.HasPrefix(q, "www."):
	default:
		return false, false
	}

	// Host only. A track title in the path could contain the word "spotify"
	// without the link being one — the question here is where it points.
	host := q
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	// Suffix match with the dot, so "notspotify.com" is not Spotify while
	// "open.spotify.com" is.
	isSpotify := host == "spotify.com" ||
		strings.HasSuffix(host, ".spotify.com") ||
		host == "spotify.link" ||
		strings.HasSuffix(host, ".spotify.link")

	return true, isSpotify
}
