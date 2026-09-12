package poller

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

const (
	// catchUpDays is how far back a fresh subscription looks.
	//
	// Three, against the poller's seven. The poll window is wide to survive
	// missed polls (D15); this one answers a different question — "did they put
	// something out just now" — and a week-old release is not that. Somebody
	// subscribing today did not ask for a backlog.
	catchUpDays = 3

	// maxCatchUp bounds one subscription's worth. An artist can release three
	// singles in three days, and the notifier paces a chat at one message a
	// second, so this is about the reader rather than about rate limits: more
	// than a few messages for pressing one button reads as a mistake.
	maxCatchUp = 3

	// feedCacheTTL is how long the catch-up window is reused.
	//
	// Subscribing to five artists in a row should not fetch 360 KB five times.
	// Fifteen minutes is far shorter than the daily cadence at which this feed
	// actually changes, and short enough that a release appearing mid-session
	// is picked up by the next subscribe.
	feedCacheTTL = 15 * time.Minute
)

// CatchUpStore is the extra storage the catch-up path needs beyond Store.
type CatchUpStore interface {
	QueueCatchUp(ctx context.Context, chatID int64, releaseGroupMBID string) (bool, error)
}

// feedCache holds one recent catch-up window.
type feedCache struct {
	mu       sync.Mutex
	releases []listenbrainz.Release
	fetched  time.Time
}

// CatchUp queues anything the artist released in the last few days for one
// subscriber, so that subscribing to somebody who put out an album on Friday is
// worth doing on Sunday rather than only from the next poll onward.
//
// Returns how many notifications were queued.
//
// Never fatal to the caller: a subscription that succeeded must not be reported
// as failed because a courtesy lookup did not work.
func (p *Poller) CatchUp(ctx context.Context, chatID int64, artistMBID string) (int, error) {
	if p.catchUp == nil {
		return 0, nil
	}

	releases, err := p.catchUpWindow(ctx)
	if err != nil {
		return 0, err
	}

	matches := p.recentBy(releases, artistMBID)
	if len(matches) == 0 {
		return 0, nil
	}

	queued := 0
	for i := range matches {
		rel := &matches[i]

		// notify=true, not false. If this release is new to us, the poller will
		// skip it forever after (a known release group is a no-op), so the
		// fan-out here is the only chance every *other* subscriber has of
		// hearing about it. The new subscriber is included, since they are
		// subscribed by the time this runs.
		out, err := p.store.Record(ctx, storage.NewRelease{
			ReleaseGroupMBID: rel.ReleaseGroupMBID,
			ArtistMBID:       artistMBID,
			Title:            rel.Title,
			PrimaryType:      rel.PrimaryType,
			ReleaseDate:      rel.ReleaseDate,
			CoverURL:         rel.CoverURL,
		}, true)
		if err != nil {
			p.log.Warn("could not record a catch-up release",
				"release_group", rel.ReleaseGroupMBID, "err", err)
			continue
		}
		if out.Created {
			// Announced to everyone as a new release, which it is — we learned
			// of it just now, and the wording matches what every other
			// subscriber is about to receive.
			queued += out.Notified
			continue
		}

		// Already known, so the fan-out happened without this subscriber. Their
		// row is queued as a catch-up, and the message says "recent" rather
		// than "new" — they can see the date, and calling a three-day-old
		// release new is a small lie that costs trust in the rest of it.
		added, err := p.catchUp.QueueCatchUp(ctx, chatID, rel.ReleaseGroupMBID)
		if err != nil {
			p.log.Warn("could not queue a catch-up notification",
				"release_group", rel.ReleaseGroupMBID, "err", err)
			continue
		}
		if added {
			queued++
		}
	}
	return queued, nil
}

// recentBy returns the artist's releases in the window, newest first, capped.
func (p *Poller) recentBy(releases []listenbrainz.Release, artistMBID string) []listenbrainz.Release {
	today := p.now().UTC().Truncate(24 * time.Hour)
	cutoff := today.AddDate(0, 0, -catchUpDays)

	var out []listenbrainz.Release
	for i := range releases {
		rel := &releases[i]
		if !rel.Notifiable() {
			continue
		}
		day := rel.ReleaseDate.UTC().Truncate(24 * time.Hour)
		// The same future guard the poll uses: announcing an album before it
		// exists is the one mistake a release bot cannot walk back.
		if day.After(today) || day.Before(cutoff) {
			continue
		}
		for _, mbid := range rel.ArtistMBIDs {
			if mbid == artistMBID {
				out = append(out, *rel)
				break
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ReleaseDate.After(out[j].ReleaseDate) })
	if len(out) > maxCatchUp {
		out = out[:maxCatchUp]
	}
	return out
}

// catchUpWindow returns the recent feed, fetching it at most once per TTL.
func (p *Poller) catchUpWindow(ctx context.Context) ([]listenbrainz.Release, error) {
	p.feed3.mu.Lock()
	defer p.feed3.mu.Unlock()

	if p.feed3.releases != nil && p.now().Sub(p.feed3.fetched) < feedCacheTTL {
		return p.feed3.releases, nil
	}

	releases, err := p.feed.FreshReleases(ctx, catchUpDays)
	if err != nil {
		// Serve a stale window rather than nothing: a slightly old answer to
		// "did they release something this week" beats no answer, and the
		// alternative is a silent subscribe.
		if p.feed3.releases != nil {
			p.log.Warn("catch-up feed unavailable, using the cached window", "err", err)
			return p.feed3.releases, nil
		}
		return nil, err
	}

	p.feed3.releases = releases
	p.feed3.fetched = p.now()
	return releases, nil
}

// WithCatchUp enables the catch-up path. Optional: without it CatchUp is a
// no-op, which is what keeps the poller's own tests unchanged.
func (p *Poller) WithCatchUp(s CatchUpStore) *Poller {
	p.catchUp = s
	return p
}
