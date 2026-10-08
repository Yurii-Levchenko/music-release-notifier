// Package metrics holds every Prometheus collector in one place.
//
// The set is not "whatever was easy to instrument". It comes from SPEC §15a,
// and it is shaped by one measured fact: Friday is 48.5% of all releases and
// 5.6× an average day (C36). So the numbers that matter are the ones that move
// on a Friday — queue depth first, because it rises before anybody notices a
// delay, and the age of the oldest queued item second, because that is the
// delay a person is actually experiencing.
//
// Collectors are held in a struct and passed explicitly rather than kept in
// package-level variables. Globals would work, and are common, but they make
// two tests that both instrument something fight over the same registry.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/backup"
)

const namespace = "releaseradar"

// QueueReader is the queue state the collector reads at scrape time.
type QueueReader interface {
	QueueDepth(ctx context.Context) (pending int, oldest time.Duration, err error)
}

type Metrics struct {
	// --- delivery ---

	// DeliverySeconds measures one send, so p95 and p99 answer "how long does
	// a message take" rather than "how long did the batch take".
	DeliverySeconds prometheus.Histogram

	// Notifications counts outcomes by disposition — sent, transient,
	// permanent, bad_message — and by why the row exists: an ordinary release
	// fan-out or a catch-up queued when somebody subscribed. The ratio between
	// dispositions is the health of the delivery path, permanent is the one
	// that costs somebody their subscriptions, and the kind split is what says
	// whether the catch-up path earns its keep.
	Notifications *prometheus.CounterVec

	// BatchesAbandoned counts batches cut short because the server asked us to
	// wait. Non-zero means we are being throttled, which the disposition
	// counter alone does not show.
	BatchesAbandoned prometheus.Counter

	// --- telegram ---

	TelegramRequests   *prometheus.CounterVec
	TelegramRetryAfter prometheus.Histogram

	// CoverSends tracks what happened to the release artwork: delivered,
	// delivered after a retry, or given up on and sent as text. The
	// degraded_to_text share is the number that says whether the covers are
	// actually reaching people, which no other metric shows.
	CoverSends *prometheus.CounterVec

	// --- poller ---

	// ReleasesFound counts what the feed returned; the Friday spike should be
	// plainly visible here.
	ReleasesFound   prometheus.Counter
	ReleasesMatched prometheus.Counter
	PollSeconds     prometheus.Histogram
	PollFailures    prometheus.Counter

	// --- upstreams ---

	MusicBrainzRequests *prometheus.CounterVec
	SearchCache         *prometheus.CounterVec

	// SearchSeconds is how long one artist search took, split by where the
	// answer came from. The duration was already measured for the log line and
	// then thrown away — which left the one question the cache exists to
	// answer, how much faster a hit is, unanswerable from the outside.
	SearchSeconds *prometheus.HistogramVec

	// --- bot ---

	BotUpdates *prometheus.CounterVec

	// --- the monitoring itself ---

	// HeartbeatEnabled is 1 while the process pings the external dead-man's
	// switch. That switch is the only alarm that survives the whole machine
	// going down, and on 16.09 a misspelled .env key turned it off twice in
	// one evening without a sound — so whether it is on is itself worth an
	// alert.
	HeartbeatEnabled prometheus.Gauge
}

// New registers every collector on reg and returns them.
func New(reg prometheus.Registerer) *Metrics {
	factory := promauto{reg}

	m := &Metrics{
		DeliverySeconds: factory.histogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "notification_delivery_seconds",
			Help:      "Time to deliver one notification, per attempt.",
			// Telegram sends land well under a second when healthy; the long
			// buckets exist to make a degraded upstream visible rather than
			// lumping everything into +Inf.
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),
		Notifications: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_total",
			Help:      "Delivery attempts by outcome.",
		}, "disposition", "kind"),
		BatchesAbandoned: factory.counter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "delivery_batches_abandoned_total",
			Help:      "Batches cut short because the server asked us to wait.",
		}),

		TelegramRequests: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "telegram_requests_total",
			Help:      "Telegram Bot API results by status code.",
		}, "code"),
		TelegramRetryAfter: factory.histogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "telegram_retry_after_seconds",
			Help:      "retry_after values Telegram asked us to honor.",
			Buckets:   []float64{1, 5, 15, 30, 60, 300, 900},
		}),

		CoverSends: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "cover_sends_total",
			Help:      "Release artwork outcomes: ok, retried_ok, degraded_to_text.",
		}, "outcome"),

		ReleasesFound: factory.counter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "poller_releases_found_total",
			Help:      "Releases returned by the feed.",
		}),
		ReleasesMatched: factory.counter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "poller_releases_matched_total",
			Help:      "Releases matching a tracked artist.",
		}),
		PollSeconds: factory.histogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "poller_duration_seconds",
			Help:      "Time for one complete poll.",
			Buckets:   []float64{1, 5, 15, 30, 60, 120, 300},
		}),
		PollFailures: factory.counter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "poller_failures_total",
			Help:      "Polls that ended in an error.",
		}),

		MusicBrainzRequests: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "musicbrainz_requests_total",
			Help:      "MusicBrainz results by status code.",
		}, "code"),
		SearchCache: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "artist_search_total",
			Help:      "Artist searches by where the answer came from.",
		}, "source"),

		SearchSeconds: factory.histogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "artist_search_seconds",
			Help:      "Time to answer one artist search, by where the answer came from.",
			// Spanning four orders of magnitude on purpose: a cache hit is
			// milliseconds, a cold MusicBrainz call is hundreds of them, and a
			// call that survived retries is tens of seconds. One bucket set has
			// to make all three legible.
			Buckets: []float64{0.005, 0.02, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}, "source"),

		BotUpdates: factory.counterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "bot_updates_total",
			Help:      "Telegram updates handled, by kind.",
		}, "kind"),

		HeartbeatEnabled: factory.gauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "heartbeat_enabled",
			Help:      "1 if the external dead-man's switch is being pinged, 0 if it is disabled.",
		}),
	}

	m.initSeries()
	return m
}

// initSeries creates the series for label sets that are fixed and known.
//
// A CounterVec publishes nothing until a label value is used, so a dashboard
// panel reads "No data" rather than zero until the first event — and for
// something like a dropped recipient, the first event is the one you least want
// to learn about from a panel that was blank until then. rate() over a series
// that does not exist yet returns nothing either.
//
// Only sets that are genuinely closed are seeded. Status codes are not: an
// invented row for a code that never occurred is a claim, not a zero.
func (m *Metrics) initSeries() {
	for _, disposition := range []string{"sent", "transient", "permanent", "bad_message"} {
		for _, kind := range []string{"release", "catch_up"} {
			m.Notifications.WithLabelValues(disposition, kind)
		}
	}
	for _, outcome := range []string{"ok", "retried_ok", "degraded_to_text"} {
		m.CoverSends.WithLabelValues(outcome)
	}
	for _, kind := range []string{"message", "callback_query", "my_chat_member", "other"} {
		m.BotUpdates.WithLabelValues(kind)
	}
}

// RegisterQueue attaches a collector that reads queue depth and age when
// Prometheus scrapes, rather than when the notifier happens to run.
//
// This distinction matters. Setting a gauge from the drain loop would mean the
// number is only correct for the instant a drain happens — and during an
// incident, when the drain loop is the thing that has stopped, the gauge would
// freeze at its last healthy value and the graph would look fine.
func (m *Metrics) RegisterQueue(reg prometheus.Registerer, queue QueueReader) error {
	return reg.Register(&queueCollector{
		queue: queue,
		pending: prometheus.NewDesc(
			namespace+"_notifications_pending",
			"Notifications waiting to be delivered.", nil, nil),
		oldest: prometheus.NewDesc(
			namespace+"_notification_queue_age_seconds",
			"Age of the oldest notification still waiting.", nil, nil),
		up: prometheus.NewDesc(
			namespace+"_queue_readable",
			"1 if the queue could be read at scrape time, 0 otherwise.", nil, nil),
	})
}

type queueCollector struct {
	queue   QueueReader
	pending *prometheus.Desc
	oldest  *prometheus.Desc
	up      *prometheus.Desc
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.oldest
	ch <- c.up
}

// Collect reads the queue. The timeout is short because a scrape must never
// hang on a struggling database — and a failed read is reported as
// queue_readable 0 rather than as a zero depth, which would look like an empty
// queue and silence exactly the alert that should fire.
func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pending, oldest, err := c.queue.QueueDepth(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}

	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(pending))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, oldest.Seconds())
}

// promauto is a tiny local version of the upstream helper. Written out because
// the upstream one panics on a duplicate registration, which is fine in main
// and awkward in a test that builds a second Metrics.
type promauto struct{ reg prometheus.Registerer }

func (p promauto) counter(opts prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(opts)
	p.register(c)
	return c
}

func (p promauto) counterVec(opts prometheus.CounterOpts, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(opts, labels)
	p.register(c)
	return c
}

func (p promauto) gauge(opts prometheus.GaugeOpts) prometheus.Gauge {
	g := prometheus.NewGauge(opts)
	p.register(g)
	return g
}

func (p promauto) histogram(opts prometheus.HistogramOpts) prometheus.Histogram {
	h := prometheus.NewHistogram(opts)
	p.register(h)
	return h
}

func (p promauto) histogramVec(opts prometheus.HistogramOpts, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(opts, labels)
	p.register(h)
	return h
}

func (p promauto) register(c prometheus.Collector) {
	if p.reg == nil {
		return
	}
	// A duplicate registration means two Metrics on one registry, which is a
	// programming error in main and harmless in a test. Ignoring it keeps
	// tests from having to thread a fresh registry through every helper.
	_ = p.reg.Register(c)
}

// Nop returns metrics attached to nothing, for tests and for code paths that
// have no registry. Every method still works, so no call site needs a nil
// check — a metric that has to be guarded eventually gets guarded wrongly.
func Nop() *Metrics { return New(nil) }

// PollStateReader reads when the poller last succeeded.
type PollStateReader interface {
	LastSuccessfulPoll(ctx context.Context) (time.Time, bool, error)
}

// RegisterPollState publishes the timestamp of the last successful poll.
//
// It is read from the database at scrape time rather than tracked in memory,
// and that is the whole point: an in-process counter resets on every restart,
// so a fresh container would look like a poller that has never run and alert
// immediately. The database remembers across deploys, which is what makes
// "the poller has not succeeded in 25 hours" a statement about the poller
// rather than about the last time somebody deployed.
func (m *Metrics) RegisterPollState(reg prometheus.Registerer, state PollStateReader) error {
	return reg.Register(&pollStateCollector{
		state: state,
		last: prometheus.NewDesc(
			namespace+"_poller_last_success_timestamp_seconds",
			"Unix time of the last successful poll, from the database.", nil, nil),
	})
}

type pollStateCollector struct {
	state PollStateReader
	last  *prometheus.Desc
}

func (c *pollStateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.last }

func (c *pollStateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	at, found, err := c.state.LastSuccessfulPoll(ctx)
	if err != nil || !found || at.IsZero() {
		// Publish nothing rather than a zero. A zero timestamp is 1970, which
		// would make "hours since the last poll" enormous and fire the alert
		// on a database that is merely unreachable, or on a first boot before
		// any poll has happened.
		return
	}
	ch <- prometheus.MustNewConstMetric(c.last, prometheus.GaugeValue, float64(at.Unix()))
}

// BackupReader is the backup state the collector reads at scrape time.
type BackupReader interface {
	State() (backup.State, error)
}

// RegisterBackups publishes the age, size and count of the database dumps.
//
// Read at scrape time, like the queue and the poll state, and for a sharper
// reason than either: the dumps are written by a different container, so there
// is no in-process event to count. Anything this process remembered would be a
// memory of a directory listing rather than the listing.
//
// Unlike the queue there is no readable gauge here, because there is no
// ambiguous zero to guard against. A queue depth of zero reads as "nothing
// waiting, all fine" and would silence the very alert that should fire, so the
// queue has to say explicitly that it could not be read. A backup timestamp
// has no such value: absent is absent, and the alert's absent() clause catches
// an unreadable directory and an empty one alike.
func (m *Metrics) RegisterBackups(reg prometheus.Registerer, backups BackupReader) error {
	return reg.Register(&backupCollector{
		backups: backups,
		last: prometheus.NewDesc(
			namespace+"_backup_last_success_timestamp_seconds",
			"Unix time of the newest database dump on disk.", nil, nil),
		size: prometheus.NewDesc(
			namespace+"_backup_size_bytes",
			"Size of the newest database dump.", nil, nil),
		count: prometheus.NewDesc(
			namespace+"_backups_retained",
			"How many dumps are currently kept.", nil, nil),
	})
}

type backupCollector struct {
	backups           BackupReader
	last, size, count *prometheus.Desc
}

func (c *backupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.last
	ch <- c.size
	ch <- c.count
}

func (c *backupCollector) Collect(ch chan<- prometheus.Metric) {
	st, err := c.backups.State()
	if err != nil {
		// Directory unreadable. Publish nothing at all, so the alert fires on
		// absent() — a zero here would claim there are no backups, which is a
		// different and much more alarming statement than "we could not look".
		return
	}

	// Count is published even at zero. An empty directory is a real, readable
	// answer, and it is the one that says rotation has eaten everything or the
	// job has never run — which only reads correctly if zero can be shown.
	ch <- prometheus.MustNewConstMetric(c.count, prometheus.GaugeValue, float64(st.Count))

	if st.Newest.IsZero() {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.last, prometheus.GaugeValue, float64(st.Newest.Unix()))
	ch <- prometheus.MustNewConstMetric(c.size, prometheus.GaugeValue, float64(st.SizeBytes))
}
