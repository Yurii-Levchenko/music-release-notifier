package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
		m.Notifications.WithLabelValues(d).Inc()
	}

	if got := testutil.CollectAndCount(m.Notifications); got != 4 {
		t.Fatalf("counted %d series, want 4", got)
	}
}

// Nop has to be usable everywhere, or every call site grows a nil check and
// one of them eventually gets it wrong.
func TestNopMetricsAreSafeToUse(t *testing.T) {
	m := Nop()

	m.DeliverySeconds.Observe(0.5)
	m.Notifications.WithLabelValues("sent").Inc()
	m.BatchesAbandoned.Inc()
	m.TelegramRequests.WithLabelValues("429").Inc()
	m.TelegramRetryAfter.Observe(30)
	m.ReleasesFound.Add(1230)
	m.ReleasesMatched.Add(1)
	m.PollSeconds.Observe(12)
	m.PollFailures.Inc()
	m.MusicBrainzRequests.WithLabelValues("503").Inc()
	m.SearchCache.WithLabelValues("cache").Inc()
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

	second.Notifications.WithLabelValues("sent").Inc()

	if got := testutil.ToFloat64(second.Notifications.WithLabelValues("sent")); got != 1 {
		t.Fatalf("counter from the second Metrics recorded %v, want 1", got)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather after a duplicate registration: %v", err)
	}
}
