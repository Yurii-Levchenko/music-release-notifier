// Package notifier drains the outbox and delivers what it finds.
//
// It is the only place that decides what a delivery failure means for a
// subscription, and it does so through notify.Disposition — it never inspects a
// channel's own error type. That is what keeps Telegram out of the domain
// (SPEC.md D13).
package notifier

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

const (
	// globalRate is deliberately under Telegram's ~30 msg/s bulk ceiling
	// (SPEC.md C3). The headroom matters because the bot also answers people
	// interactively, and a 429 earned by the outbox would land on them too.
	globalRate = 25

	// perChatGap enforces Telegram's ~1 msg/s per-chat limit. It also happens to
	// be good manners: someone following five artists who all released on Friday
	// gets five messages, and one a second reads as a feed rather than a burst.
	perChatGap = time.Second

	// batchSize is one claim's worth. Small enough that a lease expiring mid-run
	// costs little, large enough to drain a Friday backlog without thrashing.
	batchSize = 50

	// idleInterval is how long to wait when the queue came back empty. Releases
	// arrive once a day, so there is nothing to gain from a tighter loop.
	idleInterval = time.Minute

	// busyInterval is the pause after a full batch: keep draining, but let the
	// rate limiters breathe.
	busyInterval = time.Second
)

// outcome is what one delivery attempt means for the rest of the batch. Most
// deliveries are self-contained; two are not, and pretending otherwise is how a
// rate-limit response turns into a rate-limit storm.
type outcome int

const (
	// outcomeDone: handled, keep going.
	outcomeDone outcome = iota
	// outcomeDropped: the recipient is unreachable for good. Their other rows
	// in this batch must not be attempted.
	outcomeDropped
	// outcomeBackOff: the server told us to wait. Abandon the batch.
	outcomeBackOff
)

// Queue is the slice of storage the notifier needs.
type Queue interface {
	Claim(ctx context.Context, limit int) ([]storage.Pending, error)
	MarkSent(ctx context.Context, id int64) error
	Retry(ctx context.Context, id int64, attempts int, retryAfter time.Duration, reason string) (gaveUp bool, err error)
	Fail(ctx context.Context, id int64, reason string) error
	DropRecipient(ctx context.Context, userID int64, reason string) error
	QueueDepth(ctx context.Context) (pending int, oldest time.Duration, err error)
}

type Notifier struct {
	queue    Queue
	channels *notify.Registry
	log      *slog.Logger

	global *rate.Limiter
	// lastSent spaces messages to the same chat. Bounded by the number of users
	// seen since start, which for this product is small; if it ever is not, the
	// per-chat state belongs in Redis alongside a shared limiter.
	lastSent map[int64]time.Time

	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// beat reports that the drain loop is running, for the health registry.
	beat    func()
	metrics *metrics.Metrics
}

func New(queue Queue, channels *notify.Registry, log *slog.Logger) *Notifier {
	return &Notifier{
		queue:    queue,
		channels: channels,
		log:      log,
		global:   rate.NewLimiter(globalRate, globalRate),
		lastSent: make(map[int64]time.Time),
		now:      time.Now,
		sleep:    sleepCtx,
		beat:     func() {},
		metrics:  metrics.Nop(),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// WithMetrics attaches collectors.
func (n *Notifier) WithMetrics(m *metrics.Metrics) *Notifier {
	if m != nil {
		n.metrics = m
	}
	return n
}

// ReportProgressTo registers a callback invoked after each drain attempt. An
// empty queue still counts: "nothing to send" is the normal state most of the
// day, and treating it as silence would alert every night.
func (n *Notifier) ReportProgressTo(beat func()) {
	if beat != nil {
		n.beat = beat
	}
}

// Run drains the queue until ctx is canceled.
func (n *Notifier) Run(ctx context.Context) error {
	if len(n.channels.Kinds()) == 0 {
		// Nothing can be delivered. Say so once rather than looping in silence:
		// a queue that fills with nobody draining it is the kind of thing that
		// is discovered weeks later.
		n.log.Warn("notifier idle: no delivery channels are configured")
		<-ctx.Done()
		return nil
	}

	n.log.Info("notifier started", "channels", n.channels.Kinds(), "rate_per_second", globalRate)

	// A timer plus select, the same shape the bot and the poller use.
	// Cancellation arrives as a channel receive rather than as an error value,
	// so a clean shutdown never has to be expressed as "there was an error and
	// we returned nil" — the shape that hides a genuinely swallowed error.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			n.log.Info("notifier stopping")
			return nil
		case <-timer.C:
		}

		sent, err := n.drainOnce(ctx)
		n.beat()
		if err != nil && ctx.Err() == nil {
			// A failed drain is not fatal: the lease expires and the rows come
			// back. Log it and wait rather than taking the process down.
			n.log.Error("drain failed", "err", err)
		}

		wait := idleInterval
		if sent >= batchSize {
			wait = busyInterval
		}
		timer.Reset(wait)
	}
}

// drainOnce claims a batch and delivers it. Returns how many were handled.
func (n *Notifier) drainOnce(ctx context.Context) (int, error) {
	batch, err := n.queue.Claim(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	n.forgetIdleChats()

	depth, oldest, depthErr := n.queue.QueueDepth(ctx)
	if depthErr != nil {
		n.log.Warn("could not read queue depth", "err", depthErr)
	}
	n.log.Info("draining notifications",
		"claimed", len(batch), "pending", depth, "oldest", oldest.Round(time.Second))

	// dropped collects recipients found unreachable during this batch, so their
	// remaining rows are not attempted. DropRecipient has already marked those
	// rows skipped in the database; sending anyway would spend rate-limit budget
	// on a chat we know is gone, and earn error responses for it.
	dropped := make(map[int64]bool)

	for i := range batch {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		p := &batch[i]

		if dropped[p.UserID] {
			n.log.Debug("skipping notification for a recipient dropped in this batch",
				"notification_id", p.ID, "user_id", p.UserID)
			continue
		}

		switch n.deliver(ctx, p) {
		case outcomeDropped:
			dropped[p.UserID] = true

		case outcomeBackOff:
			// Telegram asked us to wait. Continuing through the batch would earn
			// one 429 per remaining row and escalate to a much longer lockout —
			// the exact behavior honoring retry_after is meant to avoid. The
			// remaining rows keep their leases and come back on their own.
			n.metrics.BatchesAbandoned.Inc()
			n.log.Warn("upstream asked us to slow down; abandoning the rest of this batch",
				"delivered", i, "abandoned", len(batch)-i-1)
			return i, nil

		case outcomeDone:
		}
	}
	return len(batch), nil
}

// forgetIdleChats drops per-chat timing state that can no longer delay
// anything. Without this the map grows for the life of the process, holding one
// entry per chat ever messaged.
func (n *Notifier) forgetIdleChats() {
	cutoff := n.now().Add(-perChatGap)
	for chatID, last := range n.lastSent {
		if last.Before(cutoff) {
			delete(n.lastSent, chatID)
		}
	}
}

// deliver sends one notification, records the result, and reports what that
// result means for the rest of the batch.
func (n *Notifier) deliver(ctx context.Context, p *storage.Pending) outcome {
	log := n.log.With(
		"notification_id", p.ID,
		"chat_id", p.ChatID,
		"attempt", p.Attempts,
	)

	channel, err := n.channels.For("telegram")
	if err != nil {
		// The build cannot deliver to this kind at all. Retrying will not help,
		// but neither is the message wrong — leave it queued for a build that
		// can.
		log.Error("no channel to deliver on", "err", err)
		return outcomeDone
	}

	if err := n.waitForSlot(ctx, p.ChatID); err != nil {
		// Context canceled; the lease expires and the row returns.
		return outcomeDone
	}

	start := n.now()
	sendErr := channel.Send(ctx, notify.Recipient{
		UserID:  p.UserID,
		Kind:    channel.Kind(),
		Address: strconv.FormatInt(p.ChatID, 10),
	}, notify.Release{
		ID:          p.ReleaseID,
		ArtistName:  p.ArtistName,
		Title:       p.Title,
		PrimaryType: p.PrimaryType,
		ReleaseDate: p.ReleaseDate,
		CoverURL:    p.CoverURL,
		InfoURL:     p.InfoURL,
		Links: notify.ArtistLinks{
			Spotify:    p.Spotify,
			YouTube:    p.YouTube,
			AppleMusic: p.AppleMusic,
			Instagram:  p.Instagram,
		},
	})
	n.lastSent[p.ChatID] = n.now()
	n.metrics.DeliverySeconds.Observe(n.now().Sub(start).Seconds())

	if sendErr == nil {
		n.metrics.Notifications.WithLabelValues("sent", kindLabel(p)).Inc()
		if err := n.queue.MarkSent(ctx, p.ID); err != nil {
			// The message went out. Failing to record that means it will be sent
			// again when the lease expires — worth an error, not a retry.
			log.Error("delivered but could not mark sent", "err", err)
			return outcomeDone
		}
		log.Info("notification sent", "artist", p.ArtistName, "title", p.Title)
		return outcomeDone
	}

	disposition, retryAfter := notify.DispositionOf(sendErr)
	switch disposition {
	case notify.Permanent:
		n.metrics.Notifications.WithLabelValues("permanent", kindLabel(p)).Inc()
		// The chat is gone for good. Drop the subscriptions rather than
		// rediscovering this on every release for the rest of time.
		log.Info("recipient is unreachable, dropping subscriptions", "err", sendErr)
		if err := n.queue.DropRecipient(ctx, p.UserID, sendErr.Error()); err != nil {
			log.Error("could not drop recipient", "err", err)
			// The drop did not stick, so the rest of this batch cannot be
			// treated as already handled.
			return outcomeDone
		}
		return outcomeDropped

	case notify.BadMessage:
		n.metrics.Notifications.WithLabelValues("bad_message", kindLabel(p)).Inc()
		// Our bug, not their fault. The subscription stays: a malformed caption
		// must not cost somebody the artists they follow.
		log.Error("message rejected, dropping this notification only", "err", sendErr)
		if err := n.queue.Fail(ctx, p.ID, sendErr.Error()); err != nil {
			log.Error("could not mark failed", "err", err)
		}
		return outcomeDone

	default: // notify.Transient
		n.metrics.Notifications.WithLabelValues("transient", kindLabel(p)).Inc()
		gaveUp, err := n.queue.Retry(ctx, p.ID, p.Attempts, retryAfter, sendErr.Error())
		if err != nil {
			log.Error("could not reschedule", "err", err)
			return outcomeDone
		}
		if gaveUp {
			log.Error("giving up on notification", "attempts", p.Attempts, "err", sendErr)
			return outcomeDone
		}
		log.Warn("delivery failed, will retry",
			"retry_after", retryAfter.Round(time.Second), "err", sendErr)

		if retryAfter > 0 {
			// A server-supplied delay is a rate limit, not a hiccup: it applies
			// to the connection, not to this one message.
			return outcomeBackOff
		}
		return outcomeDone
	}
}

// waitForSlot honors both rate limits: the global bucket and the per-chat gap.
func (n *Notifier) waitForSlot(ctx context.Context, chatID int64) error {
	if last, ok := n.lastSent[chatID]; ok {
		if gap := perChatGap - n.now().Sub(last); gap > 0 {
			if err := n.sleep(ctx, gap); err != nil {
				return err
			}
		}
	}
	return n.global.Wait(ctx)
}

// kindLabel reports why the notification exists, which is a different question
// from how old the release is and is not visible anywhere else once the row is
// sent. It is the number that says whether the catch-up path is delivering
// anything worth having.
func kindLabel(p *storage.Pending) string {
	if p.CatchUp {
		return "catch_up"
	}
	return "release"
}
