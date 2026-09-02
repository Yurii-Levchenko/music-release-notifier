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
)

const (
	defaultBaseURL = "https://musicbrainz.org/ws/2"

	// MusicBrainz allows one request per second per IP and returns 503 when you
	// exceed it (SPEC.md C25). 1.0 with a burst of 1 keeps us just inside.
	requestsPerSecond = 1.0

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
		httpClient: &http.Client{Timeout: 20 * time.Second},
		baseURL:    defaultBaseURL,
		userAgent:  ua,
		limiter:    rate.NewLimiter(rate.Limit(requestsPerSecond), 1),
		log:        log,
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

	body, err := c.get(ctx, endpoint)
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
func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Blocks until the global limiter allows a request, or ctx expires.
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("musicbrainz: wait for rate limiter: %w", err)
		}

		body, retryable, err := c.doOnce(ctx, endpoint)
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
func (c *Client) doOnce(ctx context.Context, endpoint string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, false, fmt.Errorf("musicbrainz: build request: %w", err)
	}
	// Required. Without it MusicBrainz answers 503 (SPEC.md C25).
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Timeouts and connection resets are worth another go.
		return nil, true, fmt.Errorf("musicbrainz: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
