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

	depth, oldest, depthErr := n.queue.QueueDepth(ctx)
	if depthErr != nil {
		n.log.Warn("could not read queue depth", "err", depthErr)
	}
	n.log.Info("draining notifications",
		"claimed", len(batch), "pending", depth, "oldest", oldest.Round(time.Second))

	for i := range batch {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		n.deliver(ctx, &batch[i])
	}
	return len(batch), nil
}

// deliver sends one notification and records the outcome.
func (n *Notifier) deliver(ctx context.Context, p *storage.Pending) {
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
		return
	}

	if err := n.waitForSlot(ctx, p.ChatID); err != nil {
		return // context canceled; the lease expires and the row returns
	}

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
	})
	n.lastSent[p.ChatID] = n.now()

	if sendErr == nil {
		if err := n.queue.MarkSent(ctx, p.ID); err != nil {
			// The message went out. Failing to record that means it will be sent
			// again when the lease expires — worth an error, not a retry.
			log.Error("delivered but could not mark sent", "err", err)
			return
		}
		log.Info("notification sent", "artist", p.ArtistName, "title", p.Title)
		return
	}

	disposition, retryAfter := notify.DispositionOf(sendErr)
	switch disposition {
	case notify.Permanent:
		// The chat is gone for good. Drop the subscriptions rather than
		// rediscovering this on every release for the rest of time.
		log.Info("recipient is unreachable, dropping subscriptions", "err", sendErr)
		if err := n.queue.DropRecipient(ctx, p.UserID, sendErr.Error()); err != nil {
			log.Error("could not drop recipient", "err", err)
		}

	case notify.BadMessage:
		// Our bug, not their fault. The subscription stays: a malformed caption
		// must not cost somebody the artists they follow.
		log.Error("message rejected, dropping this notification only", "err", sendErr)
		if err := n.queue.Fail(ctx, p.ID, sendErr.Error()); err != nil {
			log.Error("could not mark failed", "err", err)
		}

	default: // notify.Transient
		gaveUp, err := n.queue.Retry(ctx, p.ID, p.Attempts, retryAfter, sendErr.Error())
		if err != nil {
			log.Error("could not reschedule", "err", err)
			return
		}
		if gaveUp {
			log.Error("giving up on notification", "attempts", p.Attempts, "err", sendErr)
			return
		}
		log.Warn("delivery failed, will retry",
			"retry_after", retryAfter.Round(time.Second), "err", sendErr)
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
