// Package listenbrainz reads the fresh-releases feed.
//
// This is the heartbeat of the whole project (SPEC.md D2). One request a day
// returns every release in the window across the entire ecosystem, and the
// matching against tracked artists happens locally — so the cost does not grow
// with the number of artists anyone subscribes to. The alternative, asking a
// per-artist endpoint N times, is what made the Spotify design unworkable.
package listenbrainz

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
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.listenbrainz.org/1"

	// The feed is one large body — 90 days came back as 12 MB — so the cap is
	// generous but present.
	maxResponseBytes = 64 << 20

	maxAttempts = 4
)

// ErrNoUserAgent mirrors the MusicBrainz client: ListenBrainz needs no auth, but
// identifying the application is basic courtesy to a volunteer-funded service
// and is what gets us contacted rather than blocked if we misbehave.
var ErrNoUserAgent = errors.New("listenbrainz: User-Agent must identify the application and carry a contact address")

// Release is one entry of the feed, reduced to what the poller needs.
type Release struct {
	ReleaseGroupMBID string
	ReleaseMBID      string
	// ArtistMBIDs can hold several ids for a collaboration; a subscription to
	// any one of them should match.
	ArtistMBIDs []string
	ArtistName  string
	Title       string
	// PrimaryType is Album, Single, EP — or Broadcast, Other, or empty. The feed
	// carries all of them; filtering is the caller's job.
	PrimaryType string
	// SecondaryType qualifies the primary one: Compilation, Live, Remix,
	// Soundtrack, Demo, Mixtape/Street, DJ-mix, Interview, Audiobook,
	// Spokenword — or empty, which is the usual case.
	//
	// The field is absent from most entries rather than null, which is how it
	// went unnoticed: a dump of one release's keys does not show it. Present on
	// 9.7% of what the primary-type filter accepts (measured 10.09.2026 over a
	// 1660-release window).
	//
	// Singular, unlike MusicBrainz's own array. The feed flattens it, so a
	// "Live Compilation" arrives as one of the two. Accepted: either value is
	// enough to make the decision this drives.
	SecondaryType string
	ReleaseDate   time.Time
	// CoverURL is built from the response, never fetched. Empty for roughly a
	// quarter of releases (SPEC.md C27b).
	CoverURL string
}

type Client struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
	log        *slog.Logger
}

func New(userAgent string, log *slog.Logger) (*Client, error) {
	ua := strings.TrimSpace(userAgent)
	if ua == "" || !strings.Contains(ua, "@") {
		return nil, fmt.Errorf("%w (got %q)", ErrNoUserAgent, userAgent)
	}
	return &Client{
		// Generous: the 90-day body was 12 MB, and a daily job can afford to wait.
		httpClient: &http.Client{Timeout: 2 * time.Minute},
		baseURL:    defaultBaseURL,
		userAgent:  ua,
		log:        log,
	}, nil
}

type freshReleasesResponse struct {
	Payload struct {
		TotalCount int `json:"total_count"`
		Releases   []struct {
			ArtistCreditName string   `json:"artist_credit_name"`
			ArtistMBIDs      []string `json:"artist_mbids"`
			// caa_id is an Internet Archive identifier and exceeds 32 bits.
			CAAID          int64  `json:"caa_id"`
			CAAReleaseMBID string `json:"caa_release_mbid"`
			ReleaseDate    string `json:"release_date"`
			ReleaseGroup   string `json:"release_group_mbid"`
			ReleaseMBID    string `json:"release_mbid"`
			ReleaseName    string `json:"release_name"`
			PrimaryType    string `json:"release_group_primary_type"`
			SecondaryType  string `json:"release_group_secondary_type"`
		} `json:"releases"`
	} `json:"payload"`
}

// FreshReleases returns everything released in the past `days` days.
//
// past=true&future=false is not optional. days=N is a window of ±N days, not a
// look-back (SPEC.md C24): days=3 came back with releases three days into the
// future. Without the flags the poller would announce albums before they exist.
func (c *Client) FreshReleases(ctx context.Context, days int) ([]Release, error) {
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("listenbrainz: days must be 1..90, got %d", days)
	}

	q := url.Values{}
	q.Set("days", strconv.Itoa(days))
	q.Set("past", "true")
	q.Set("future", "false")
	endpoint := c.baseURL + "/explore/fresh-releases/?" + q.Encode()

	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var parsed freshReleasesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("listenbrainz: decode fresh releases: %w", err)
	}

	// The whole window arrives in one body; there is no pagination to follow
	// (SPEC.md C24b), so a short response means a short window, not a first page.
	out := make([]Release, 0, len(parsed.Payload.Releases))
	for i := range parsed.Payload.Releases {
		r := &parsed.Payload.Releases[i]

		// A release with no release group cannot be deduplicated (SPEC.md D7),
		// and one with no artist cannot be matched. Either way it is unusable.
		if r.ReleaseGroup == "" || len(r.ArtistMBIDs) == 0 {
			continue
		}
		date, ok := parseReleaseDate(r.ReleaseDate)
		if !ok {
			continue
		}

		out = append(out, Release{
			ReleaseGroupMBID: r.ReleaseGroup,
			ReleaseMBID:      r.ReleaseMBID,
			ArtistMBIDs:      r.ArtistMBIDs,
			ArtistName:       r.ArtistCreditName,
			Title:            r.ReleaseName,
			PrimaryType:      r.PrimaryType,
			SecondaryType:    r.SecondaryType,
			ReleaseDate:      date,
			CoverURL:         coverURL(r.CAAReleaseMBID, r.CAAID),
		})
	}

	c.log.Debug("fresh releases fetched",
		"days", days, "total", parsed.Payload.TotalCount, "usable", len(out))
	return out, nil
}

// coverURL builds the Cover Art Archive address from fields already in the
// response. We never request it: Telegram fetches the image itself when the
// notification is sent, so this costs zero HTTP calls (SPEC.md C27).
func coverURL(releaseMBID string, caaID int64) string {
	if releaseMBID == "" || caaID == 0 {
		return ""
	}
	return "https://coverartarchive.org/release/" + releaseMBID + "/front-250"
}

// parseReleaseDate accepts the full date the feed uses. MusicBrainz elsewhere
// allows year- and month-only precision; a release we cannot place on a day
// cannot be checked against "is it out yet", so it is skipped rather than
// guessed at.
func parseReleaseDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, retryable, err := c.doOnce(ctx, endpoint)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
		if attempt < maxAttempts {
			// Jittered, for the same reason as the MusicBrainz client: clients
			// that fail together should not retry together. A daily job can
			// afford to wait longer than an interactive search.
			backoff := jitter(time.Duration(1<<(attempt-1)) * 5 * time.Second)
			c.log.Warn("listenbrainz request failed, retrying",
				"attempt", attempt, "backoff", backoff, "err", err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, fmt.Errorf("listenbrainz: giving up after %d attempts: %w", maxAttempts, lastErr)
}

func (c *Client) doOnce(ctx context.Context, endpoint string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, false, fmt.Errorf("listenbrainz: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("listenbrainz: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if readErr != nil {
			return nil, true, fmt.Errorf("listenbrainz: read body: %w", readErr)
		}
		return b, false, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("listenbrainz: http %d", resp.StatusCode)
	default:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, false, fmt.Errorf("listenbrainz: http %d: %s",
			resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := int64(d) / 2
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread+1))
}

// Which releases are worth telling somebody about.
//
// This lives with the feed rather than in the poller because two workers now
// ask the question — the daily poll and the catch-up run when somebody
// subscribes — and two copies of a filter that must agree is how they stop
// agreeing.
const (
	typeAlbum  = "Album"
	typeSingle = "Single"
	typeEP     = "EP"
)

// excludedSecondaryTypes are qualifiers that make a release not "new music by
// an artist you follow", whatever its primary type says.
//
// SPEC D3 excluded compilations from the start and the poller was not honoring
// it, because the filter looked only at the primary type: an official
// Brazil-only compilation of unreleased Ariana Grande tracks was announced as a
// new album on 10.09.2026. Measured over a 1660-release window, 9.7% of
// everything the primary-type filter accepts carries a secondary type.
//
// What is excluded and what is kept is a taste decision, so it is a list
// rather than a condition:
//
//   - Compilation, Demo — old material, repackaged or unfinished. D3.
//   - Interview, Audiobook, Spokenword — not music.
//   - DJ-mix — mostly other people's tracks.
//
// Deliberately kept, because each is a real release somebody following the
// artist would want to hear about: Live (a new performance), Remix (official
// new versions), Soundtrack (new work), Mixtape/Street (a primary release
// format in hip-hop, not a lesser one).
var excludedSecondaryTypes = map[string]bool{
	"Compilation": true,
	"Demo":        true,
	"Interview":   true,
	"Audiobook":   true,
	"Spokenword":  true,
	"DJ-mix":      true,
}

// NotifiableType reports whether the primary type is one we announce. The feed
// also carries Broadcast, Other and empty types.
func (r Release) NotifiableType() bool {
	return r.PrimaryType == typeAlbum || r.PrimaryType == typeSingle || r.PrimaryType == typeEP
}

// NotifiableSecondary reports whether the secondary type is worth a message.
// Empty is the usual case and always passes.
//
// An unfamiliar value passes too: the feed's vocabulary is MusicBrainz's, which
// grows, and defaulting to "drop" would let a new qualifier silently swallow
// releases.
func (r Release) NotifiableSecondary() bool {
	return !excludedSecondaryTypes[r.SecondaryType]
}

// Notifiable is both checks, for callers that do not need to tell them apart.
func (r Release) Notifiable() bool {
	return r.NotifiableType() && r.NotifiableSecondary()
}
