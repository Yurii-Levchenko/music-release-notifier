// Package poller turns the ListenBrainz feed into queued notifications.
//
// It runs once a day. That is enough because the feed is a window, not a
// stream: each poll re-reads the last several days, and the release group
// unique constraint makes overlapping windows free (SPEC.md D15). A missed
// poll costs nothing as long as the next one happens inside the window.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

const (
	// Source name in poll_state.
	sourceName = "listenbrainz"

	// windowDays is the look-back. Seven rather than one because the feed
	// filters on release_date, not on when the record was entered (SPEC.md
	// C24c): a release dated Monday and entered by a volunteer on Wednesday
	// would never appear in a one-day window. It also absorbs missed polls,
	// which have no backfill.
	windowDays = 7

	// notifiableTypes are the only release kinds worth a message (SPEC.md D3).
	// The feed also carries Broadcast, Other and empty types.
	typeAlbum  = "Album"
	typeSingle = "Single"
	typeEP     = "EP"
)

// Feed is the slice of ListenBrainz the poller needs.
type Feed interface {
	FreshReleases(ctx context.Context, days int) ([]listenbrainz.Release, error)
}

// Store is the slice of storage the poller needs.
type Store interface {
	TrackedArtistMBIDs(ctx context.Context) (map[string]struct{}, error)
	Record(ctx context.Context, rel storage.NewRelease, notify bool) (storage.FanOut, error)
	PollState(ctx context.Context, source string) (storage.PollState, bool, error)
	RecordPoll(ctx context.Context, source string, pollErr error) error
}

type Poller struct {
	feed     Feed
	store    Store
	interval time.Duration
	log      *slog.Logger
	// now is injectable so tests can place a release in the future without
	// waiting for tomorrow.
	now func() time.Time

	// beat reports that the poll loop is running, for the health registry.
	beat    func()
	metrics *metrics.Metrics

	// links is optional; nil disables the backfill.
	links LinkBackfill
}

func New(feed Feed, store Store, interval time.Duration, log *slog.Logger) *Poller {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Poller{
		feed: feed, store: store, interval: interval, log: log,
		now: time.Now, beat: func() {}, metrics: metrics.Nop(),
	}
}

// Stats describe one poll.
type Stats struct {
	Fetched   int  // usable entries in the feed
	Matched   int  // entries by an artist somebody follows
	Recorded  int  // releases seen for the first time
	Notified  int  // notifications queued
	Seeded    bool // first run: recorded a baseline without notifying
	TrackedBy int  // how many distinct artists are followed
}

// Run polls on a schedule until ctx is canceled.
// ReportProgressTo registers a callback invoked after each completed poll,
// successful or not: the health signal is "the poller is running", and a poll
// that failed and logged it is still a running poller. A source outage is the
// upstream metric's job, not the dead-man's switch's.
func (p *Poller) ReportProgressTo(beat func()) {
	if beat != nil {
		p.beat = beat
	}
}

func (p *Poller) Run(ctx context.Context) error {
	// Wait out the remainder of the interval if a previous process already
	// polled recently. Without this, a container that restarts often would poll
	// on every deploy — harmless, because the writes are idempotent, but it
	// spends someone else's bandwidth for nothing.
	delay := p.initialDelay(ctx)
	if delay > 0 {
		p.log.Info("poller waiting for the next due time", "delay", delay.Round(time.Minute))
	}

	// Once at startup, and not only on the poll cycle. The backfill does not
	// touch the release feed, so there is no reason to make it wait out a
	// day-long interval — a deploy that adds links should show them, not show
	// them tomorrow. Bounded by linksPerPoll and by links_fetched_at, so a
	// restart loop cannot turn this into a burst.
	p.backfillLinks(ctx)

	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			p.log.Info("poller stopping")
			return nil
		case <-timer.C:
		}

		stats, err := p.PollOnce(ctx)
		p.beat()
		if err != nil {
			// A failed poll is not fatal. The window is wide enough that the
			// next attempt still covers everything this one missed.
			p.log.Error("poll failed", "err", err)
		} else {
			p.log.Info("poll complete",
				"fetched", stats.Fetched,
				"tracked_artists", stats.TrackedBy,
				"matched", stats.Matched,
				"recorded", stats.Recorded,
				"notified", stats.Notified,
				"seeded", stats.Seeded)
		}
		timer.Reset(p.interval)
	}
}

func (p *Poller) initialDelay(ctx context.Context) time.Duration {
	state, found, err := p.store.PollState(ctx, sourceName)
	if err != nil {
		p.log.Warn("could not read poll state, polling now", "err", err)
		return 0
	}
	if !found || state.LastPolledAt.IsZero() {
		return 0
	}
	if elapsed := p.now().Sub(state.LastPolledAt); elapsed < p.interval {
		return p.interval - elapsed
	}
	return 0
}

// PollOnce reads the feed and records what it finds. Exported so it can be
// triggered deliberately rather than only by the clock.
// LinkBackfill fills in artist streaming links that were never looked up.
//
// This exists because links are fetched at subscribe time, which leaves every
// artist subscribed to before the feature existed permanently without them —
// a feature that only works for future subscriptions is half a feature. The
// poller is the right home: it is the worker whose whole job is periodic
// maintenance against a rate-limited upstream.
type LinkBackfill interface {
	ArtistsMissingLinks(ctx context.Context, limit int) ([]string, error)
	SetArtistLinks(ctx context.Context, mbid string, links storage.ArtistLinks) error
	ArtistLinks(ctx context.Context, mbid string) (musicbrainz.Links, error)
}

// linksPerPoll bounds the backfill. MusicBrainz allows one request a second,
// and this work is never urgent: a missing link costs a row in a message, not
// the message. Small and daily beats a burst that competes with somebody's
// search.
const linksPerPoll = 5

// WithLinkBackfill enables the backfill. Optional: without it the poller
// behaves exactly as before, which is what keeps every existing poller test
// unchanged.
func (p *Poller) WithLinkBackfill(b LinkBackfill) *Poller {
	p.links = b
	return p
}

// backfillLinks looks up a few artists' links. Never fatal: this runs after
// the poll that actually matters, and a MusicBrainz outage must not make a
// successful poll look failed.
func (p *Poller) backfillLinks(ctx context.Context) {
	if p.links == nil {
		return
	}

	pending, err := p.links.ArtistsMissingLinks(ctx, linksPerPoll)
	if err != nil {
		p.log.Warn("could not list artists missing links", "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	p.log.Info("backfilling artist links", "artists", len(pending))

	for _, mbid := range pending {
		if ctx.Err() != nil {
			return
		}

		links, err := p.links.ArtistLinks(ctx, mbid)
		if err != nil {
			// Leaves links_fetched_at null, so the next poll tries again.
			p.log.Warn("link lookup failed", "mbid", mbid, "err", err)
			continue
		}
		if err := p.links.SetArtistLinks(ctx, mbid, storage.ArtistLinks{
			Spotify:    links.Spotify,
			YouTube:    links.YouTube,
			AppleMusic: links.AppleMusic,
		}); err != nil {
			p.log.Warn("could not store links", "mbid", mbid, "err", err)
			continue
		}
		p.log.Info("artist links stored", "mbid", mbid, "found", !links.Empty())
	}
}

// WithMetrics attaches collectors.
func (p *Poller) WithMetrics(m *metrics.Metrics) *Poller {
	if m != nil {
		p.metrics = m
	}
	return p
}

func (p *Poller) PollOnce(ctx context.Context) (Stats, error) {
	start := p.now()
	stats, err := p.poll(ctx)

	// Duration is recorded even for a failed poll: a poll that failed after
	// two minutes and one that failed instantly are different problems.
	p.metrics.PollSeconds.Observe(p.now().Sub(start).Seconds())
	p.metrics.ReleasesFound.Add(float64(stats.Fetched))
	p.metrics.ReleasesMatched.Add(float64(stats.Matched))
	if err != nil {
		p.metrics.PollFailures.Inc()
	}

	// After the poll, never instead of it: detecting releases is the job, and
	// filling in links is housekeeping.
	p.backfillLinks(ctx)

	// Record the attempt either way: the gap between last_polled_at and
	// last_ok_at is how an alert learns the poller has been failing.
	if recErr := p.store.RecordPoll(ctx, sourceName, err); recErr != nil {
		p.log.Warn("could not record poll state", "err", recErr)
	}
	return stats, err
}

func (p *Poller) poll(ctx context.Context) (Stats, error) {
	var stats Stats

	tracked, err := p.store.TrackedArtistMBIDs(ctx)
	if err != nil {
		return stats, err
	}
	stats.TrackedBy = len(tracked)
	if len(tracked) == 0 {
		// Nobody follows anyone. Fetching the feed would be pure waste.
		p.log.Debug("no tracked artists, skipping fetch")
		return stats, nil
	}

	// Whether this is the first run decides if anyone gets messaged. With a
	// seven-day window, everything the first poll finds is new to us and old to
	// the user; announcing it all would be a flood on day one.
	//
	// The signal is "has a poll ever succeeded", not "does the releases table
	// have rows". Those look equivalent and are not: a first poll where nobody
	// happened to release anything records nothing, which would leave the
	// poller seeding forever — and the first genuine release would be silently
	// swallowed instead of sent. Found by running it.
	state, found, err := p.store.PollState(ctx, sourceName)
	if err != nil {
		return stats, err
	}
	firstRun := !found || state.LastOKAt.IsZero()
	stats.Seeded = firstRun
	notify := !firstRun

	releases, err := p.feed.FreshReleases(ctx, windowDays)
	if err != nil {
		return stats, err
	}
	stats.Fetched = len(releases)

	today := p.now().UTC().Truncate(24 * time.Hour)

	for i := range releases {
		rel := &releases[i]

		if !notifiableType(rel.PrimaryType) {
			continue
		}
		// future=false should have handled this upstream; keep the assert,
		// because announcing an album before it exists is the one mistake a
		// release bot cannot walk back (SPEC.md FR-2.5).
		//
		// Both sides are truncated to a day. A release date is a calendar date,
		// not an instant, and comparing one against a timestamp would treat
		// anything released "today" as still in the future for most of the day.
		if rel.ReleaseDate.UTC().Truncate(24 * time.Hour).After(today) {
			p.log.Warn("skipping a future-dated release",
				"release_group", rel.ReleaseGroupMBID, "date", rel.ReleaseDate)
			continue
		}

		artistMBID, ok := firstTracked(rel.ArtistMBIDs, tracked)
		if !ok {
			continue
		}
		stats.Matched++

		out, err := p.store.Record(ctx, storage.NewRelease{
			ReleaseGroupMBID: rel.ReleaseGroupMBID,
			ArtistMBID:       artistMBID,
			Title:            rel.Title,
			PrimaryType:      rel.PrimaryType,
			ReleaseDate:      rel.ReleaseDate,
			CoverURL:         rel.CoverURL,
		}, notify)
		if err != nil {
			// One bad row must not abandon the rest of the feed.
			p.log.Error("could not record release",
				"release_group", rel.ReleaseGroupMBID, "err", err)
			continue
		}
		if out.Created {
			stats.Recorded++
			stats.Notified += out.Notified
			if notify {
				p.log.Info("new release",
					"artist", rel.ArtistName,
					"title", rel.Title,
					"type", rel.PrimaryType,
					"date", rel.ReleaseDate.Format(time.DateOnly),
					"notified", out.Notified)
			}
		}
	}

	if stats.Seeded {
		p.log.Info("first poll: recorded a baseline without notifying anyone",
			"recorded", stats.Recorded)
	}
	return stats, nil
}

func notifiableType(t string) bool {
	return t == typeAlbum || t == typeSingle || t == typeEP
}

// firstTracked returns the first artist on the release that somebody follows.
//
// A collaboration lists several artists, and one subscription is enough to earn
// one notification. Attributing the release to the matched artist — rather than
// to the first credited one — keeps it in the right person's list.
func firstTracked(mbids []string, tracked map[string]struct{}) (string, bool) {
	for _, id := range mbids {
		if _, ok := tracked[id]; ok {
			return id, true
		}
	}
	return "", false
}

// ErrNotConfigured is returned when the poller is asked to run without a feed.
var ErrNotConfigured = errors.New("poller: no feed configured")

func (p *Poller) String() string {
	return fmt.Sprintf("poller(%s, every %s)", sourceName, p.interval)
}
