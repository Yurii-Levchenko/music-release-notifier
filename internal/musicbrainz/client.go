// Package musicbrainz is a minimal client for the MusicBrainz web service.
//
// Only artist search is implemented: release detection comes from ListenBrainz
// (SPEC.md D2), so MusicBrainz is used purely to turn a name a human typed into
// an MBID we can subscribe to.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"
)

const (
	defaultBaseURL = "https://musicbrainz.org/ws/2"

	// MusicBrainz allows one request per second per IP and returns 503 when you
	// exceed it (SPEC.md C25).
	//
	// One request every 1.1 seconds rather than exactly one per second. "Just
	// inside" turned out to mean "on the boundary": their limiter counts a
	// window, so a burst pinned to exactly 1/s lands two requests inside one
	// of their seconds often enough to matter. Measured 10.09.2026 — four
	// lookups spaced 1.5 s still collected 503s, and the app's own backfill
	// spent three minutes on work worth thirty seconds.
	//
	// The 10% costs nothing: nothing here is interactive except search, and a
	// search waits on one request, not on the interval.
	requestInterval = 1100 * time.Millisecond

	// searchAttemptTimeout bounds one interactive request. A human is watching
	// a "Шукаю…" placeholder, and there is a stale-cache fallback behind this.
	searchAttemptTimeout = 20 * time.Second

	// lookupAttemptTimeout bounds one background request, and is deliberately
	// far larger.
	//
	// The artist lookup with `inc=url-rels` is expensive on their side: TTFB
	// was measured at 8.7 s for a cold one, against 0.13 s once warm, and
	// identical from the host and from inside Docker — so it is their compute,
	// not our network. Against a 20 s budget that produced
	// "Client.Timeout exceeded while awaiting headers", five retries, and a
	// lookup that eventually succeeded anyway. Nobody is waiting on this, so
	// waiting is cheaper than retrying.
	lookupAttemptTimeout = 60 * time.Second

	// 503 is returned for rate limiting, for a missing User-Agent, and for
	// genuine server load — all with the same generic "server is currently
	// busy" body, so they cannot be told apart. Verified empirically: every
	// 503 seen during development resolved on retry, so 503 is always
	// transient and never means "no results".
	//
	// Five rather than four: a live search gave up after ~7 seconds and the
	// same query succeeded two minutes later. Since MusicBrainz 503s track
	// load, a slightly wider window converts a visible failure into a slower
	// answer. It stays bounded, because a search has to remain interactive —
	// beyond this the stale-cache fallback takes over.
	maxAttempts = 5

	// Response bodies for a 5-artist search are a few KB; this is a sanity cap,
	// not a tuning knob.
	maxResponseBytes = 4 << 20
)

// ErrEmptyQuery means the query had no searchable characters left after
// cleaning. MusicBrainz answers a blank query with 400, so we stop earlier.
var ErrEmptyQuery = errors.New("musicbrainz: empty search query")

// Artist is the subset of a search hit worth showing a human.
//
// There is deliberately no image: Cover Art Archive covers releases and release
// groups, not artists, so an artist picker cannot show a photo without a
// separate service. What MusicBrainz does return disambiguates better than a
// picture would anyway — see SPEC.md C32.
type Artist struct {
	MBID           string   `json:"mbid"`
	Name           string   `json:"name"`
	SortName       string   `json:"sort_name,omitempty"`
	Score          int      `json:"score"`
	Type           string   `json:"type,omitempty"`    // Group | Person | Choir | ...
	Country        string   `json:"country,omitempty"` // ISO 3166-1 alpha-2
	Disambiguation string   `json:"disambiguation,omitempty"`
	Begin          string   `json:"begin,omitempty"` // life-span begin, may be a bare year
	End            string   `json:"end,omitempty"`
	Tags           []string `json:"tags,omitempty"` // most-voted tags, effectively genres
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
	limiter    *rate.Limiter
	log        *slog.Logger
	metrics    *metrics.Metrics
}

// ErrNoUserAgent means the client was built without an identifying User-Agent.
//
// MusicBrainz answers requests with no User-Agent with 403 and an explicit
// message ("the application you are using has not identified itself"), and
// answers a *generic* one such as Go-http-client or curl with 503 and a vague
// "server is currently busy". Neither is retryable-into-working, so failing at
// construction beats discovering it one request at a time.
var ErrNoUserAgent = errors.New("musicbrainz: User-Agent must identify the application and carry a contact address")

// New builds a client. userAgent must identify the application and carry a
// contact address, e.g. "ReleaseRadar/0.1 ( you@example.com )".
func New(userAgent string, log *slog.Logger) (*Client, error) {
	ua := strings.TrimSpace(userAgent)
	// A contact address is what MusicBrainz asks for, and its absence is the
	// difference between being throttled and being emailed about a problem.
	if ua == "" || !strings.Contains(ua, "@") {
		return nil, fmt.Errorf("%w (got %q)", ErrNoUserAgent, userAgent)
	}
	return &Client{
		// No Timeout on the client: it would cap every call at one value, and
		// an interactive search and a background lookup want very different
		// budgets. The deadline is applied per attempt instead, so a retry
		// gets a full budget rather than the remains of a shared one.
		httpClient: &http.Client{},
		baseURL:    defaultBaseURL,
		userAgent:  ua,
		limiter:    rate.NewLimiter(rate.Every(requestInterval), 1),
		log:        log,
		metrics:    metrics.Nop(),
	}, nil
}

// searchResponse mirrors only the fields we use.
type searchResponse struct {
	Count   int `json:"count"`
	Artists []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SortName       string `json:"sort-name"`
		Score          int    `json:"score"`
		Type           string `json:"type"`
		Country        string `json:"country"`
		Disambiguation string `json:"disambiguation"`
		LifeSpan       struct {
			Begin string `json:"begin"`
			End   string `json:"end"`
		} `json:"life-span"`
		Tags []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"tags"`
	} `json:"artists"`
}

// SearchArtist returns candidates for a name a human typed, best match first.
//
// An empty result set is not an error: MusicBrainz answers an unmatched query
// with 200 and count 0.
func (c *Client) SearchArtist(ctx context.Context, query string, limit int) ([]Artist, error) {
	cleaned := escapeLucene(query)
	if cleaned == "" {
		return nil, ErrEmptyQuery
	}
	if limit <= 0 || limit > 25 {
		limit = 5
	}

	q := url.Values{}
	q.Set("query", cleaned)
	q.Set("fmt", "json")
	q.Set("limit", fmt.Sprint(limit))
	endpoint := c.baseURL + "/artist?" + q.Encode()

	body, err := c.get(ctx, endpoint, searchAttemptTimeout)
	if err != nil {
		return nil, err
	}

	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("musicbrainz: decode search response: %w", err)
	}

	out := make([]Artist, 0, len(parsed.Artists))
	// Index rather than range-copy: each element is ~160 bytes.
	for i := range parsed.Artists {
		a := &parsed.Artists[i]
		out = append(out, Artist{
			MBID:           a.ID,
			Name:           a.Name,
			SortName:       a.SortName,
			Score:          a.Score,
			Type:           a.Type,
			Country:        a.Country,
			Disambiguation: a.Disambiguation,
			Begin:          a.LifeSpan.Begin,
			End:            a.LifeSpan.End,
			Tags:           topTags(a.Tags),
		})
	}
	return out, nil
}

// get performs one rate-limited request, retrying 503 and 5xx with backoff.
func (c *Client) get(ctx context.Context, endpoint string, attemptTimeout time.Duration) ([]byte, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Blocks until the global limiter allows a request, or ctx expires.
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("musicbrainz: wait for rate limiter: %w", err)
		}

		body, retryable, err := c.doOnce(ctx, endpoint, attemptTimeout)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}

		if attempt < maxAttempts {
			// 1s, 2s, 4s, 8s, each jittered. Jitter matters more than the
			// curve: without it every client that failed at the same moment
			// retries at the same moment, and a service that is already
			// struggling gets a synchronized wave instead of a trickle.
			backoff := jitter(time.Duration(1<<(attempt-1)) * time.Second)
			c.log.Warn("musicbrainz request failed, retrying",
				"attempt", attempt, "backoff", backoff, "err", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, fmt.Errorf("musicbrainz: giving up after %d attempts: %w", maxAttempts, lastErr)
}

// jitter spreads a backoff by ±25% so concurrent clients stop retrying in
// lockstep. Without it, everyone who failed at the same moment retries at the
// same moment, and a service that is already struggling gets a synchronized
// wave instead of a trickle.
//
// math/rand rather than crypto/rand is deliberate: this is scheduling, not
// security, and predictability here costs nothing.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 2 // total width of the window: d/4 either side
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1))
}

// doOnce reports whether the failure is worth retrying.
func (c *Client) doOnce(ctx context.Context, endpoint string, timeout time.Duration) (body []byte, retryable bool, err error) {
	// Per attempt, so a retry after a stall gets its own full budget. A
	// deadline on the whole call would give the last attempt whatever the
	// earlier ones left, which is the opposite of what a retry is for.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, false, fmt.Errorf("musicbrainz: build request: %w", err)
	}
	// Required. Without it MusicBrainz answers 503 (SPEC.md C25).
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Timeouts and connection resets are worth another go. Counted as
		// "transport" rather than a status, because the request may never have
		// reached MusicBrainz at all.
		c.metrics.MusicBrainzRequests.WithLabelValues("transport").Inc()
		return nil, true, fmt.Errorf("musicbrainz: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bucketed, not the raw code: an unbounded label fed from an upstream is
	// how a metrics backend gets a cardinality explosion. The 503 share is the
	// number that matters here (C25c).
	c.metrics.MusicBrainzRequests.WithLabelValues(statusLabel(resp.StatusCode)).Inc()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if readErr != nil {
			return nil, true, fmt.Errorf("musicbrainz: read body: %w", readErr)
		}
		return b, false, nil

	case resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("musicbrainz: http %d", resp.StatusCode)

	default:
		// 400 for a malformed query, 404, and anything else we should not hammer.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, false, fmt.Errorf("musicbrainz: http %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

// Tags that are notes between MusicBrainz editors rather than descriptions of
// the music: "fixme label mess", "todo", "needs splitting". They are ordinary
// voted tags, so they outrank real genres and end up on the card, where they
// read as nonsense to somebody choosing an artist. Seen live on あいみょん.
//
// Matching is by whole word, not by substring. Substring matching looked
// simpler and was wrong: "spam" also matches the genre "spamdexcore", and
// "check" matches "checkered pop" — a filter that hides real genres is worse
// than the noise it removes. Caught by its own test, not in review.
var (
	// housekeepingWords disqualify a tag when they appear as a whole word.
	housekeepingWords = []string{
		"fixme", "todo", "wip",
		"needs", "misattributed", "incorrect", "wrongly",
		"duplicate", "spam", "check", "verify",
	}
	// housekeepingPhrases are multi-word notes with no single telling word.
	housekeepingPhrases = []string{
		"to split", "to merge", "bad data", "label mess",
	}
)

// isHousekeeping reports whether a tag is an editor note rather than a genre.
//
// Tags are free text, so this can never be complete. That is acceptable because
// it fails gracefully both ways: an unlisted note shows one odd tag, and a
// genre wrongly matched costs one of three slots.
func isHousekeeping(tag string) bool {
	lower := strings.ToLower(tag)

	for _, phrase := range housekeepingPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	for _, word := range strings.FieldsFunc(lower, func(r rune) bool {
		// Split on anything that is not part of a word. Hyphens count as
		// separators so "post-rock" is two words, which is harmless, while
		// "fixme-label" is still caught.
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		for _, marker := range housekeepingWords {
			if word == marker {
				return true
			}
		}
	}
	return false
}

// maxTags is how many tags reach the card. MusicBrainz can attach dozens;
// three is enough to tell two same-named artists apart, and more would push the
// fields that actually disambiguate off the visible part of the message.
const maxTags = 3

// topTags returns the most-voted tag names, editor notes removed.
func topTags(tags []struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}) []string {
	if len(tags) == 0 {
		return nil
	}
	// Copy so sorting does not disturb the decoded response.
	type tag struct {
		name  string
		count int
	}
	sorted := make([]tag, 0, len(tags))
	for _, t := range tags {
		if t.Name != "" && !isHousekeeping(t.Name) {
			sorted = append(sorted, tag{t.Name, t.Count})
		}
	}
	// Simple selection: n is 3, so an O(n*len) pass beats pulling in sort.
	out := make([]string, 0, maxTags)
	for len(out) < maxTags && len(sorted) > 0 {
		best := 0
		for i := 1; i < len(sorted); i++ {
			if sorted[i].count > sorted[best].count {
				best = i
			}
		}
		out = append(out, sorted[best].name)
		sorted = append(sorted[:best], sorted[best+1:]...)
	}
	return out
}

// luceneSpecials are the characters the MusicBrainz query parser treats as
// syntax. Confirmed live: `artist:(foo OR bar) AND ][` is parsed as a field
// query and returns 275,000 unrelated results, so raw user input reaching the
// parser produces confusing output rather than a useful search.
const luceneSpecials = `+-&|!(){}[]^"~*?:\/`

// escapeLucene makes a human-typed name literal to the query parser, and
// collapses whitespace.
//
// Escaping rather than stripping: a name like "Panic! At The Disco" or "AC/DC"
// keeps every character, so the index can still match on it. Bare syntax such
// as "][" becomes a literal search for "][" instead of a parse artifact.
func escapeLucene(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s) + 8)

	lastWasSpace := false
	wroteAny := false
	for _, r := range s {
		// Collapse runs of whitespace, including tabs and newlines pasted in.
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if wroteAny {
				lastWasSpace = true
			}
			continue
		}
		// Control characters would confuse the parser and cannot be part of a name.
		if r < 0x20 || r == 0x7f {
			continue
		}
		if lastWasSpace {
			b.WriteByte(' ')
			lastWasSpace = false
		}
		if r < 0x80 && strings.ContainsRune(luceneSpecials, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		wroteAny = true
	}
	return b.String()
}

// statusLabel buckets an HTTP status into a bounded label set.
func statusLabel(code int) string {
	switch {
	case code == http.StatusOK:
		return "200"
	case code == http.StatusServiceUnavailable:
		return "503"
	case code == http.StatusTooManyRequests:
		return "429"
	case code == http.StatusForbidden:
		return "403"
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return "other"
	}
}

// WithMetrics attaches collectors.
func (c *Client) WithMetrics(m *metrics.Metrics) *Client {
	if m != nil {
		c.metrics = m
	}
	return c
}

// LinksVersion identifies the set of link kinds pickLinks collects.
//
// Bump it when a kind is added or removed. The backfill re-resolves anything
// stored against an older version, which is what makes adding a kind a
// one-line change rather than a data migration — and what stops a new field
// from only ever appearing for artists subscribed to afterwards.
const LinksVersion = 1

// Links are the external destinations worth putting in a notification.
//
// Curated hard on purpose. A lookup returns everything MusicBrainz knows —
// 49 relations for Drake, including three Discogs entries, VIAF, WorldCat and
// two lyrics sites. A message with all of that is unreadable, and the reader
// wants exactly one thing: somewhere to press play.
type Links struct {
	Spotify    string `json:"spotify,omitempty"`
	YouTube    string `json:"youtube,omitempty"`
	AppleMusic string `json:"apple_music,omitempty"`
	// Instagram is stored as the full URL rather than the handle, so there is
	// one source of truth; the handle is derived for display.
	Instagram string `json:"instagram,omitempty"`
}

// Empty reports whether nothing usable was found.
func (l Links) Empty() bool {
	return l.Spotify == "" && l.YouTube == "" && l.AppleMusic == "" && l.Instagram == ""
}

// ArtistLinks looks up one artist's external URLs.
//
// This is a second request, not part of the search: the artist *index* carries
// no relationships at all — verified live, `inc=url-rels` is silently ignored
// on a search and the response has no relations key. At one request a second
// that makes links something to fetch once per artist and store, never
// something to resolve while rendering a card.
func (c *Client) ArtistLinks(ctx context.Context, mbid string) (Links, error) {
	if !isMBID(mbid) {
		// Also keeps anything but a UUID out of the request path.
		return Links{}, fmt.Errorf("musicbrainz: %q is not an mbid", mbid)
	}

	body, err := c.get(ctx, c.baseURL+"/artist/"+mbid+"?inc=url-rels&fmt=json", lookupAttemptTimeout)
	if err != nil {
		return Links{}, err
	}

	var payload struct {
		// The name comes from the same response, so choosing between several
		// Instagram accounts needs no extra parameter from the caller.
		Name      string         `json:"name"`
		Relations []linkRelation `json:"relations"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Links{}, fmt.Errorf("musicbrainz: decode artist links: %w", err)
	}

	return pickLinks(payload.Relations, payload.Name), nil
}

// linkRelation is one url-rel, named rather than anonymous so the selection
// logic can be tested without an HTTP round trip.
type linkRelation struct {
	Type  string `json:"type"`
	Ended bool   `json:"ended"`
	URL   struct {
		Resource string `json:"resource"`
	} `json:"url"`
}

// pickLinks chooses one URL per service from MusicBrainz's relation list.
//
// Two things it has to get right.
//
// **Ended relations are skipped.** Snoop Dogg's list includes a Google+
// profile and a dead iTunes page, both flagged ended. Handing somebody a link
// to Google+ is worse than showing no link at all.
//
// **Duplicates resolve to the first live match.** An artist can have several
// YouTube channels or Apple Music pages and MusicBrainz returns them in no
// meaningful order, so this is arbitrary-but-stable rather than correct. Good
// enough for "go listen"; not good enough to present as canonical.
//
// Matching is on host as well as relation type, because "free streaming"
// covers Spotify, Deezer and Pandora alike, and "streaming" covers Apple,
// Tidal, Amazon and Qobuz.
func pickLinks(relations []linkRelation, artistName string) Links {
	var l Links
	var instagram []string

	for _, r := range relations {
		if r.Ended {
			continue
		}
		resource := r.URL.Resource

		switch {
		case l.Spotify == "" && strings.Contains(resource, "open.spotify.com/artist/"):
			l.Spotify = resource
		case l.YouTube == "" && r.Type == "youtube":
			l.YouTube = resource
		case l.AppleMusic == "" && strings.Contains(resource, "music.apple.com/"):
			l.AppleMusic = resource
		case strings.Contains(resource, "instagram.com/"):
			// Collected rather than taken, because an artist can have several
			// and the first is not necessarily theirs. See pickInstagram.
			instagram = append(instagram, resource)
		}
	}

	l.Instagram = pickInstagram(instagram, artistName)
	return l
}

// pickInstagram chooses between several Instagram accounts on one artist.
//
// "First match wins" is wrong here, and provably so: Kendrick Lamar has two
// live `social network` relations, instagram.com/jojoruski and
// instagram.com/kendricklamar, and nothing in the API separates them — same
// type, neither ended, no attributes, no begin dates. Taking the first would
// have put a stranger's handle in his release notifications.
//
// So the only available signal is the handle itself: prefer one that matches
// the artist's name once both are reduced to letters and digits. When nothing
// matches — Drake's account is @champagnepapi, and あいみょん's handle is not
// even in the same script as her name — the first is used, which is the best
// available guess rather than a correct answer.
func pickInstagram(candidates []string, artistName string) string {
	if len(candidates) == 0 {
		return ""
	}

	want := normalizeHandle(artistName)
	if want != "" {
		for _, url := range candidates {
			handle := normalizeHandle(InstagramHandle(url))
			if handle == "" {
				continue
			}
			if handle == want || strings.Contains(handle, want) || strings.Contains(want, handle) {
				return url
			}
		}
	}
	return candidates[0]
}

// InstagramHandle extracts the account name from an Instagram profile URL.
// Returns "" for anything that is not a plain profile link.
func InstagramHandle(rawURL string) string {
	const marker = "instagram.com/"
	i := strings.Index(rawURL, marker)
	if i < 0 {
		return ""
	}

	handle := rawURL[i+len(marker):]

	// Query and fragment first, then slashes. The other order rejects
	// ".../snoopdogg/?hl=en" as a multi-segment path, which it is not — caught
	// by its own test.
	if cut := strings.IndexAny(handle, "?#"); cut >= 0 {
		handle = handle[:cut]
	}
	handle = strings.Trim(handle, "/")

	// A profile URL has exactly one path segment. Anything deeper is a post, a
	// reel or a story, none of which is an account.
	if handle == "" || strings.Contains(handle, "/") {
		return ""
	}
	return handle
}

// normalizeHandle reduces a name or handle to comparable letters and digits,
// so "Kendrick Lamar" and "kendricklamar" match and "jojoruski" does not.
func normalizeHandle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isMBID reports whether s is shaped like a MusicBrainz UUID.
func isMBID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
