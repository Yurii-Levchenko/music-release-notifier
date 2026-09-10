package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

const (
	tracked1 = "aaaaaaaa-0000-4000-8000-000000000001"
	tracked2 = "bbbbbbbb-0000-4000-8000-000000000002"
	stranger = "cccccccc-0000-4000-8000-000000000003"
)

var fixedNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

type fakeFeed struct {
	calls    int
	releases []listenbrainz.Release
	err      error
	gotDays  int
}

func (f *fakeFeed) FreshReleases(_ context.Context, days int) ([]listenbrainz.Release, error) {
	f.calls++
	f.gotDays = days
	return f.releases, f.err
}

type recorded struct {
	rel    storage.NewRelease
	notify bool
}

type fakeStore struct {
	tracked map[string]struct{}

	records    []recorded
	recordErr  error
	failOnMBID string // makes Record fail for one specific release group

	pollErrs   []error // what RecordPoll was told, in order
	pollCalls  int
	stateFound bool
	state      storage.PollState

	trackedErr error
}

func (s *fakeStore) TrackedArtistMBIDs(context.Context) (map[string]struct{}, error) {
	return s.tracked, s.trackedErr
}

func (s *fakeStore) Record(_ context.Context, rel storage.NewRelease, notify bool) (storage.FanOut, error) {
	if s.recordErr != nil {
		return storage.FanOut{}, s.recordErr
	}
	if s.failOnMBID != "" && rel.ReleaseGroupMBID == s.failOnMBID {
		return storage.FanOut{}, errors.New("simulated write failure")
	}
	s.records = append(s.records, recorded{rel: rel, notify: notify})
	out := storage.FanOut{ReleaseID: int64(len(s.records)), Created: true}
	if notify {
		out.Notified = 1
	}
	return out, nil
}

func (s *fakeStore) PollState(context.Context, string) (storage.PollState, bool, error) {
	return s.state, s.stateFound, nil
}

func (s *fakeStore) RecordPoll(_ context.Context, _ string, pollErr error) error {
	s.pollCalls++
	s.pollErrs = append(s.pollErrs, pollErr)
	return nil
}

func testPoller(feed Feed, store Store) *Poller {
	p := New(feed, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = func() time.Time { return fixedNow }
	return p
}

func release(rgMBID, primaryType string, artistMBIDs []string, date time.Time) listenbrainz.Release {
	return listenbrainz.Release{
		ReleaseGroupMBID: rgMBID,
		ArtistMBIDs:      artistMBIDs,
		ArtistName:       "Someone",
		Title:            "Some Album",
		PrimaryType:      primaryType,
		ReleaseDate:      date,
	}
}

// The feed parses dates with time.DateOnly, so a real release date is midnight
// UTC. Fixtures mirror that rather than carrying a stray time component.
func day(offset int) time.Time {
	return fixedNow.UTC().Truncate(24*time.Hour).AddDate(0, 0, offset)
}

func yesterday() time.Time { return day(-1) }

// Only Album, Single and EP are worth a message (SPEC.md D3). The live feed also
// carries Broadcast, Other and empty types, and a Radiohead "Broadcast" entry
// would be a wrong notification, not a missing one.
func TestPollFiltersReleaseTypes(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-album", "Album", []string{tracked1}, yesterday()),
		release("rg-single", "Single", []string{tracked1}, yesterday()),
		release("rg-ep", "EP", []string{tracked1}, yesterday()),
		release("rg-broadcast", "Broadcast", []string{tracked1}, yesterday()),
		release("rg-other", "Other", []string{tracked1}, yesterday()),
		release("rg-empty", "", []string{tracked1}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Recorded != 3 {
		t.Fatalf("recorded %d releases, want 3 (Album, Single, EP)", stats.Recorded)
	}
	for _, r := range store.records {
		if !notifiableType(r.rel.PrimaryType) {
			t.Fatalf("recorded a %q release", r.rel.PrimaryType)
		}
	}
}

// The window looks back seven days, so the very first poll finds a week of
// releases that are new to us and old to the user. Announcing them all would be
// a flood on day one.
func TestFirstPollSeedsWithoutNotifying(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-1", "Album", []string{tracked1}, yesterday()),
		release("rg-2", "Single", []string{tracked1}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: false}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if !stats.Seeded {
		t.Fatal("first poll was not reported as a seed")
	}
	if stats.Recorded != 2 {
		t.Fatalf("recorded %d, want 2 — the baseline must still be stored", stats.Recorded)
	}
	if stats.Notified != 0 {
		t.Fatalf("first poll queued %d notifications, want 0", stats.Notified)
	}
	for _, r := range store.records {
		if r.notify {
			t.Fatal("a release was recorded with notify=true during the seed")
		}
	}
}

// Found by running the real thing. The first poll fetched 1230 releases and
// matched none of them, because the one tracked artist had a quiet week — so it
// recorded nothing.
//
// While "first run" was defined as "the releases table is empty", that poll left
// the poller permanently in seeding mode, and the artist's next actual release
// would have been recorded silently instead of sent. A release bot that
// swallows the first release it ever sees is worse than one that does not run.
//
// The signal is now "has a poll ever succeeded", which is true regardless of
// whether anything matched.
func TestSeedingEndsAfterAPollThatMatchedNothing(t *testing.T) {
	// First run: nobody released anything the poller cares about.
	store := &fakeStore{
		tracked:    map[string]struct{}{tracked1: {}},
		stateFound: false,
	}
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-someone-else", "Album", []string{stranger}, yesterday()),
	}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("first PollOnce: %v", err)
	}
	if !stats.Seeded || stats.Recorded != 0 {
		t.Fatalf("first poll: seeded=%v recorded=%d, want true and 0", stats.Seeded, stats.Recorded)
	}

	// That poll succeeded, so the baseline is established even though it is
	// empty. RecordPoll would have stamped last_ok_at.
	store.stateFound = true
	store.state = storage.PollState{LastOKAt: fixedNow}

	// Now the artist actually releases something.
	feed.releases = []listenbrainz.Release{
		release("rg-the-real-one", "Album", []string{tracked1}, yesterday()),
	}

	stats, err = testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if stats.Seeded {
		t.Fatal("still seeding after a successful poll; the first real release would be swallowed")
	}
	if stats.Notified != 1 {
		t.Fatalf("notified %d, want 1 — this is the release the user signed up for", stats.Notified)
	}
}

func TestSubsequentPollNotifies(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-1", "Album", []string{tracked1}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Seeded {
		t.Fatal("a later poll was reported as a seed")
	}
	if stats.Notified != 1 {
		t.Fatalf("notified %d, want 1", stats.Notified)
	}
}

// future=false should have handled this upstream, but announcing an album before
// it exists is the one mistake a release bot cannot walk back.
func TestPollSkipsFutureDatedReleases(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-future", "Album", []string{tracked1}, day(3)),
		release("rg-today", "Album", []string{tracked1}, day(0)),
		release("rg-past", "Album", []string{tracked1}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Recorded != 2 {
		t.Fatalf("recorded %d, want 2 — the future-dated one must be skipped", stats.Recorded)
	}
	for _, r := range store.records {
		if r.rel.ReleaseGroupMBID == "rg-future" {
			t.Fatal("recorded a release dated in the future")
		}
	}
	// Today counts as released: the feed works in whole days.
	var sawToday bool
	for _, r := range store.records {
		if r.rel.ReleaseGroupMBID == "rg-today" {
			sawToday = true
		}
	}
	if !sawToday {
		t.Fatal("a release dated today was treated as future")
	}
}

// A collaboration credits several artists. One subscription is enough, and the
// release must be filed under the artist the user actually follows — otherwise
// it lands in nobody's list.
func TestPollAttributesToTheTrackedArtist(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-collab", "Album", []string{stranger, tracked2, stranger}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked2: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	if _, err := testPoller(feed, store).PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(store.records) != 1 {
		t.Fatalf("recorded %d releases, want 1", len(store.records))
	}
	if got := store.records[0].rel.ArtistMBID; got != tracked2 {
		t.Fatalf("attributed to %q, want the followed artist %q", got, tracked2)
	}
}

func TestPollIgnoresUntrackedArtists(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-1", "Album", []string{stranger}, yesterday()),
	}}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if stats.Matched != 0 || stats.Recorded != 0 {
		t.Fatalf("matched=%d recorded=%d, want 0 and 0", stats.Matched, stats.Recorded)
	}
}

// Fetching a multi-megabyte feed to match against nothing is pure waste, and it
// is someone else's bandwidth.
func TestPollSkipsFetchWhenNobodyFollowsAnyone(t *testing.T) {
	feed := &fakeFeed{}
	store := &fakeStore{tracked: map[string]struct{}{}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if feed.calls != 0 {
		t.Fatalf("fetched the feed %d times with no tracked artists", feed.calls)
	}
	if stats.Fetched != 0 {
		t.Fatalf("fetched=%d, want 0", stats.Fetched)
	}
}

// One unwritable row must not abandon the rest of the feed. A single malformed
// release should cost one notification, not a day of them.
func TestPollContinuesAfterAWriteFailure(t *testing.T) {
	feed := &fakeFeed{releases: []listenbrainz.Release{
		release("rg-ok-1", "Album", []string{tracked1}, yesterday()),
		release("rg-bad", "Album", []string{tracked1}, yesterday()),
		release("rg-ok-2", "Album", []string{tracked1}, yesterday()),
	}}
	store := &fakeStore{
		tracked:    map[string]struct{}{tracked1: {}},
		stateFound: true,
		state:      storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)},
		failOnMBID: "rg-bad",
	}

	stats, err := testPoller(feed, store).PollOnce(context.Background())
	if err != nil {
		t.Fatalf("a single bad row aborted the poll: %v", err)
	}
	if stats.Recorded != 2 {
		t.Fatalf("recorded %d, want the 2 that were writable", stats.Recorded)
	}
}

// The window is what makes overlapping polls safe and late MusicBrainz entries
// visible (SPEC.md C24c, D15). Narrowing it silently loses releases.
func TestPollUsesTheSevenDayWindow(t *testing.T) {
	feed := &fakeFeed{}
	store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}

	if _, err := testPoller(feed, store).PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if feed.gotDays != 7 {
		t.Fatalf("asked for %d days, want 7", feed.gotDays)
	}
}

// The gap between last_polled_at and last_ok_at is how an alert learns the
// poller has been failing, so a failed attempt has to be recorded too.
func TestPollRecordsStateOnSuccessAndFailure(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}
		if _, err := testPoller(&fakeFeed{}, store).PollOnce(context.Background()); err != nil {
			t.Fatalf("PollOnce: %v", err)
		}
		if store.pollCalls != 1 || store.pollErrs[0] != nil {
			t.Fatalf("recorded %d polls, first err=%v", store.pollCalls, store.pollErrs[0])
		}
	})

	t.Run("failure", func(t *testing.T) {
		wantErr := errors.New("listenbrainz: http 503")
		store := &fakeStore{tracked: map[string]struct{}{tracked1: {}}, stateFound: true, state: storage.PollState{LastOKAt: fixedNow.Add(-24 * time.Hour)}}
		_, err := testPoller(&fakeFeed{err: wantErr}, store).PollOnce(context.Background())
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want the feed error", err)
		}
		if store.pollCalls != 1 {
			t.Fatalf("recorded %d polls, want 1", store.pollCalls)
		}
		if !errors.Is(store.pollErrs[0], wantErr) {
			t.Fatalf("recorded err = %v, want the feed error", store.pollErrs[0])
		}
	})
}

// A restart must not re-poll if a previous process already did, or a container
// that redeploys often spends someone else's bandwidth on every deploy.
func TestInitialDelayRespectsRecentPoll(t *testing.T) {
	store := &fakeStore{
		stateFound: true,
		state:      storage.PollState{LastPolledAt: fixedNow.Add(-20 * time.Minute)},
	}
	p := New(&fakeFeed{}, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = func() time.Time { return fixedNow }

	got := p.initialDelay(context.Background())
	if want := 40 * time.Minute; got != want {
		t.Fatalf("initialDelay = %v, want %v", got, want)
	}
}

func TestInitialDelayIsZeroWhenOverdueOrUnknown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for name, store := range map[string]*fakeStore{
		"never polled": {stateFound: false},
		"overdue":      {stateFound: true, state: storage.PollState{LastPolledAt: fixedNow.Add(-3 * time.Hour)}},
		"zero time":    {stateFound: true, state: storage.PollState{}},
	} {
		t.Run(name, func(t *testing.T) {
			p := New(&fakeFeed{}, store, time.Hour, logger)
			p.now = func() time.Time { return fixedNow }
			if got := p.initialDelay(context.Background()); got != 0 {
				t.Fatalf("initialDelay = %v, want 0", got)
			}
		})
	}
}

// --- link backfill ----------------------------------------------------------

type fakeBackfill struct {
	pending []string
	links   map[string]musicbrainz.Links
	stored  map[string]storage.ArtistLinks
	lookups []string

	listErr   error
	lookupErr error

	askedVersion  int
	storedVersion int
}

func (f *fakeBackfill) ArtistsMissingLinks(_ context.Context, limit, version int) ([]string, error) {
	f.askedVersion = version
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.pending) > limit {
		return f.pending[:limit], nil
	}
	return f.pending, nil
}

func (f *fakeBackfill) ArtistLinks(_ context.Context, mbid string) (musicbrainz.Links, error) {
	f.lookups = append(f.lookups, mbid)
	if f.lookupErr != nil {
		return musicbrainz.Links{}, f.lookupErr
	}
	return f.links[mbid], nil
}

func (f *fakeBackfill) SetArtistLinks(_ context.Context, mbid string, l storage.ArtistLinks, version int) error {
	f.storedVersion = version
	if f.stored == nil {
		f.stored = map[string]storage.ArtistLinks{}
	}
	f.stored[mbid] = l
	return nil
}

// Links are fetched at subscribe time, which leaves every artist subscribed to
// before the feature existed permanently without them. The backfill is what
// makes it a whole feature rather than half of one.
func TestBackfillFillsArtistsThatNeverHadALookup(t *testing.T) {
	b := &fakeBackfill{
		pending: []string{"mbid-a", "mbid-b"},
		links: map[string]musicbrainz.Links{
			"mbid-a": {
				Spotify:    "https://open.spotify.com/artist/A",
				YouTube:    "https://youtube.com/channel/A",
				AppleMusic: "https://music.apple.com/us/artist/1",
				Instagram:  "https://www.instagram.com/a/",
			},
			"mbid-b": {YouTube: "https://youtube.com/channel/B"},
		},
	}
	p := bareTestPoller().WithLinkBackfill(b)

	p.backfillLinks(context.Background())

	if len(b.stored) != 2 {
		t.Fatalf("stored %d, want 2", len(b.stored))
	}

	// Every field, not just one. Asserting only Spotify let a missing
	// Instagram field ship: the mapping to storage.ArtistLinks is written out
	// by hand, so a new link kind is exactly the thing that gets forgotten in
	// one of the two call sites.
	got := b.stored["mbid-a"]
	want := storage.ArtistLinks{
		Spotify:    "https://open.spotify.com/artist/A",
		YouTube:    "https://youtube.com/channel/A",
		AppleMusic: "https://music.apple.com/us/artist/1",
		Instagram:  "https://www.instagram.com/a/",
	}
	if got != want {
		t.Errorf("stored = %+v, want %+v", got, want)
	}
}

// An artist with no links must still be recorded as looked at, or the lookup
// repeats every poll forever against a one-request-a-second API.
func TestBackfillRecordsAnEmptyResult(t *testing.T) {
	b := &fakeBackfill{
		pending: []string{"mbid-none"},
		links:   map[string]musicbrainz.Links{"mbid-none": {}},
	}
	p := bareTestPoller().WithLinkBackfill(b)

	p.backfillLinks(context.Background())

	if _, recorded := b.stored["mbid-none"]; !recorded {
		t.Fatal("an artist with no links was not recorded as checked; it will be retried forever")
	}
}

// A failed lookup must not be recorded, so the next poll tries again.
func TestBackfillLeavesAFailedLookupUnrecorded(t *testing.T) {
	b := &fakeBackfill{
		pending:   []string{"mbid-x"},
		lookupErr: errors.New("503 service unavailable"),
	}
	p := bareTestPoller().WithLinkBackfill(b)

	p.backfillLinks(context.Background())

	if len(b.stored) != 0 {
		t.Fatalf("a failed lookup was recorded as done: %+v", b.stored)
	}
}

// The backfill is housekeeping. A MusicBrainz outage must not be able to make
// a successful poll look failed.
func TestBackfillFailureIsNotFatal(t *testing.T) {
	b := &fakeBackfill{listErr: errors.New("database unreachable")}
	p := bareTestPoller().WithLinkBackfill(b)

	p.backfillLinks(context.Background()) // must not panic

	if len(b.lookups) != 0 {
		t.Fatal("looked up artists it could not list")
	}
}

// Without a backfill configured the poller must behave exactly as before —
// nil is the normal state for every existing deployment and every other test.
func TestPollerWithoutBackfillIsInert(t *testing.T) {
	p := bareTestPoller()

	// A nil dependency must be a no-op, not a nil dereference: the poll loop
	// calls this on every cycle.
	p.backfillLinks(context.Background())

	if p.links != nil {
		t.Fatal("a backfill appeared without being configured")
	}
}

// Cancellation has to stop the loop rather than working through the batch.
func TestBackfillStopsOnCancellation(t *testing.T) {
	b := &fakeBackfill{pending: []string{"a", "b", "c"}, links: map[string]musicbrainz.Links{}}
	p := bareTestPoller().WithLinkBackfill(b)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.backfillLinks(ctx)

	if len(b.lookups) != 0 {
		t.Fatalf("performed %d lookups after cancellation", len(b.lookups))
	}
}

// bareTestPoller builds a poller with no feed or store: the backfill touches
// neither, and supplying fakes for them would only obscure what is under test.
func bareTestPoller() *Poller {
	return New(nil, nil, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A row must be stamped with the version it was resolved against, or the
// backfill cannot tell "resolved against an older set of link kinds" from
// "resolved, and has none" — and one of those has to be re-resolved while the
// other must never be.
func TestBackfillStampsTheLinksVersion(t *testing.T) {
	b := &fakeBackfill{
		pending: []string{"mbid-a"},
		links:   map[string]musicbrainz.Links{"mbid-a": {Spotify: "https://open.spotify.com/artist/A"}},
	}
	p := bareTestPoller().WithLinkBackfill(b)

	p.backfillLinks(context.Background())

	if b.askedVersion != musicbrainz.LinksVersion {
		t.Errorf("asked for version %d, want %d", b.askedVersion, musicbrainz.LinksVersion)
	}
	if b.storedVersion != musicbrainz.LinksVersion {
		t.Errorf("stored version %d, want %d", b.storedVersion, musicbrainz.LinksVersion)
	}
}
