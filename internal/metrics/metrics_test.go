package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/backup"
)

type fakeQueue struct {
	pending int
	oldest  time.Duration
	err     error
}

func (q fakeQueue) QueueDepth(context.Context) (int, time.Duration, error) {
	return q.pending, q.oldest, q.err
}

func TestQueueCollectorReportsDepthAndAge(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	if err := m.RegisterQueue(reg, fakeQueue{pending: 7, oldest: 90 * time.Second}); err != nil {
		t.Fatalf("register queue: %v", err)
	}

	expected := `
# HELP releaseradar_notification_queue_age_seconds Age of the oldest notification still waiting.
# TYPE releaseradar_notification_queue_age_seconds gauge
releaseradar_notification_queue_age_seconds 90
# HELP releaseradar_notifications_pending Notifications waiting to be delivered.
# TYPE releaseradar_notifications_pending gauge
releaseradar_notifications_pending 7
# HELP releaseradar_queue_readable 1 if the queue could be read at scrape time, 0 otherwise.
# TYPE releaseradar_queue_readable gauge
releaseradar_queue_readable 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"releaseradar_notifications_pending",
		"releaseradar_notification_queue_age_seconds",
		"releaseradar_queue_readable"); err != nil {
		t.Fatal(err)
	}
}

// The important case. An unreadable queue must not publish a depth of zero:
// zero looks like an empty queue, which would silence the very alert that
// should be firing while the database is unreachable.
func TestUnreadableQueueDoesNotLookEmpty(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	if err := m.RegisterQueue(reg, fakeQueue{err: errors.New("connection refused")}); err != nil {
		t.Fatalf("register queue: %v", err)
	}

	expected := `
# HELP releaseradar_queue_readable 1 if the queue could be read at scrape time, 0 otherwise.
# TYPE releaseradar_queue_readable gauge
releaseradar_queue_readable 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"releaseradar_queue_readable"); err != nil {
		t.Fatal(err)
	}

	// And the depth gauge must be absent rather than published as zero.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "releaseradar_notifications_pending" {
			t.Fatalf("published %s while the queue was unreadable; zero reads as an empty queue",
				f.GetName())
		}
	}
}

// Label values must come from a bounded set. A label fed straight from an
// upstream status is how a metrics backend gets a cardinality explosion.
func TestDispositionLabelsAreBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	for _, d := range []string{"sent", "transient", "permanent", "bad_message"} {
		for _, kind := range []string{"release", "catch_up"} {
			m.Notifications.WithLabelValues(d, kind).Inc()
		}
	}

	// Four dispositions times two kinds, and no more: both label sets are
	// closed, so the series count is a fixed number rather than something an
	// upstream can grow.
	if got := testutil.CollectAndCount(m.Notifications); got != 8 {
		t.Fatalf("counted %d series, want 8", got)
	}
}

// Nop has to be usable everywhere, or every call site grows a nil check and
// one of them eventually gets it wrong.
func TestNopMetricsAreSafeToUse(t *testing.T) {
	m := Nop()

	m.DeliverySeconds.Observe(0.5)
	m.Notifications.WithLabelValues("sent", "release").Inc()
	m.BatchesAbandoned.Inc()
	m.TelegramRequests.WithLabelValues("429").Inc()
	m.TelegramRetryAfter.Observe(30)
	m.ReleasesFound.Add(1230)
	m.ReleasesMatched.Add(1)
	m.PollSeconds.Observe(12)
	m.PollFailures.Inc()
	m.MusicBrainzRequests.WithLabelValues("503").Inc()
	m.SearchCache.WithLabelValues("cache").Inc()
	m.SearchSeconds.WithLabelValues("cache").Observe(0.004)
	m.BotUpdates.WithLabelValues("message").Inc()

	if m.Notifications == nil {
		t.Fatal("Nop returned incomplete metrics")
	}
}

// Two Metrics on one registry is a programming error in main and routine in a
// test. It must not panic, and the second one must still be usable — silently
// handing back collectors that record into nothing would be worse than a panic.
func TestDuplicateRegistrationStaysUsable(t *testing.T) {
	reg := prometheus.NewRegistry()
	New(reg)
	second := New(reg)

	second.Notifications.WithLabelValues("sent", "release").Inc()

	if got := testutil.ToFloat64(second.Notifications.WithLabelValues("sent", "release")); got != 1 {
		t.Fatalf("counter from the second Metrics recorded %v, want 1", got)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather after a duplicate registration: %v", err)
	}
}

// A CounterVec publishes nothing until a label is used, so a dashboard reads
// "No data" instead of zero until the first event — and for a dropped
// recipient, the first event is the one you least want to discover from a
// panel that was blank until then.
func TestClosedLabelSetsExistFromStartup(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	if got := testutil.CollectAndCount(m.Notifications); got != 8 {
		t.Errorf("notifications series at startup = %d, want 8", got)
	}
	if got := testutil.CollectAndCount(m.CoverSends); got != 3 {
		t.Errorf("cover series at startup = %d, want 3", got)
	}
	if got := testutil.CollectAndCount(m.BotUpdates); got != 4 {
		t.Errorf("bot update series at startup = %d, want 4", got)
	}

	// Status codes are deliberately not seeded: a row for a code that never
	// occurred is a claim, not a zero.
	if got := testutil.CollectAndCount(m.TelegramRequests); got != 0 {
		t.Errorf("telegram code series at startup = %d, want none invented", got)
	}
}

// The duration was already measured for the log line and discarded, which left
// the question the cache exists to answer — how much faster a hit is —
// unanswerable from outside the process.
func TestSearchDurationIsRecordedBySource(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.SearchSeconds.WithLabelValues("cache").Observe(0.004)
	m.SearchSeconds.WithLabelValues("musicbrainz").Observe(0.8)

	if got := testutil.CollectAndCount(m.SearchSeconds); got != 2 {
		t.Fatalf("counted %d series, want one per source", got)
	}

	// The bucket set has to keep the two apart. If the smallest bucket
	// swallowed a hit, the percentiles would report it taking as long as a
	// miss, and the panel would say the cache does nothing.
	buckets := bucketCounts(t, reg, "releaseradar_artist_search_seconds")

	if buckets["cache"][0.005] != 1 {
		t.Errorf("a 4 ms hit did not land in the smallest bucket: %v", buckets["cache"])
	}
	if buckets["musicbrainz"][0.005] != 0 {
		t.Errorf("an 800 ms miss landed in the 5 ms bucket: %v", buckets["musicbrainz"])
	}
	if buckets["musicbrainz"][1] != 1 {
		t.Errorf("an 800 ms miss is not counted by the 1 s bucket: %v", buckets["musicbrainz"])
	}
}

// bucketCounts reads cumulative bucket counts per source out of a registry.
func bucketCounts(t *testing.T, reg *prometheus.Registry, name string) map[string]map[float64]uint64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	out := map[string]map[float64]uint64{}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			var source string
			for _, l := range metric.GetLabel() {
				if l.GetName() == "source" {
					source = l.GetValue()
				}
			}
			counts := map[float64]uint64{}
			for _, b := range metric.GetHistogram().GetBucket() {
				counts[b.GetUpperBound()] = b.GetCumulativeCount()
			}
			out[source] = counts
		}
	}
	return out
}

type fakeBackups struct {
	state backup.State
	err   error
}

func (f fakeBackups) State() (backup.State, error) { return f.state, f.err }

// published reports which metric families the registry is currently exposing.
func published(t *testing.T, reg *prometheus.Registry) map[string]bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	return names
}

// The mirror of TestUnreadableQueueDoesNotLookEmpty, in the other direction. An
// unreadable backup directory must publish nothing at all. A zero timestamp
// would mean 1970, and "the last backup was 56 years ago" is a far louder claim
// than "we could not look" — but a zero count would be worse still, because it
// asserts there are no backups when there may be seven.
func TestUnreadableBackupDirPublishesNothing(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	if err := m.RegisterBackups(reg, fakeBackups{err: errors.New("permission denied")}); err != nil {
		t.Fatalf("register backups: %v", err)
	}

	names := published(t, reg)
	for _, name := range []string{
		"releaseradar_backup_last_success_timestamp_seconds",
		"releaseradar_backup_size_bytes",
		"releaseradar_backups_retained",
	} {
		if names[name] {
			t.Errorf("published %s for an unreadable directory", name)
		}
	}
}

// An empty directory is a readable answer, and the only one that can say
// rotation has deleted everything. So the count must appear, at zero, while the
// timestamp must not: there is no backup to date.
func TestEmptyBackupDirPublishesACountButNoTimestamp(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	if err := m.RegisterBackups(reg, fakeBackups{}); err != nil {
		t.Fatalf("register backups: %v", err)
	}

	expected := `
# HELP releaseradar_backups_retained How many dumps are currently kept.
# TYPE releaseradar_backups_retained gauge
releaseradar_backups_retained 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"releaseradar_backups_retained"); err != nil {
		t.Fatal(err)
	}

	if published(t, reg)["releaseradar_backup_last_success_timestamp_seconds"] {
		t.Error("published a backup timestamp with no dumps on disk")
	}
}

func TestBackupCollectorPublishesFreshness(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	at := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	err := m.RegisterBackups(reg, fakeBackups{state: backup.State{
		Newest: at, SizeBytes: 40400, Count: 7,
	}})
	if err != nil {
		t.Fatalf("register backups: %v", err)
	}

	// Size and timestamp must describe the same file, which is why they are
	// asserted together rather than one at a time.
	expected := `
# HELP releaseradar_backup_last_success_timestamp_seconds Unix time of the newest database dump on disk.
# TYPE releaseradar_backup_last_success_timestamp_seconds gauge
releaseradar_backup_last_success_timestamp_seconds 1.7894412e+09
# HELP releaseradar_backup_size_bytes Size of the newest database dump.
# TYPE releaseradar_backup_size_bytes gauge
releaseradar_backup_size_bytes 40400
# HELP releaseradar_backups_retained How many dumps are currently kept.
# TYPE releaseradar_backups_retained gauge
releaseradar_backups_retained 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"releaseradar_backup_last_success_timestamp_seconds",
		"releaseradar_backup_size_bytes",
		"releaseradar_backups_retained"); err != nil {
		t.Fatal(err)
	}
}
