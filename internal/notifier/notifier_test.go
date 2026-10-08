package notifier

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// --- fakes ------------------------------------------------------------------

type sentMessage struct {
	to  notify.Recipient
	rel notify.Release
}

type fakeChannel struct {
	sent []sentMessage
	err  error
	// errs lets a test return a different error per call.
	errs []error
	// during runs inside Send, standing in for whatever happens while the
	// request is on the wire — a SIGTERM, for one.
	during func(ctx context.Context)
}

func (f *fakeChannel) Kind() string { return "telegram" }

func (f *fakeChannel) Send(ctx context.Context, to notify.Recipient, rel notify.Release) error {
	if f.during != nil {
		f.during(ctx)
	}
	f.sent = append(f.sent, sentMessage{to: to, rel: rel})
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return f.err
}

type retryCall struct {
	id         int64
	attempts   int
	retryAfter time.Duration
	reason     string
}

type fakeQueue struct {
	batches [][]storage.Pending
	claims  int

	marked   []int64
	retries  []retryCall
	failed   []int64
	dropped  []int64
	gaveUpOn map[int64]bool

	claimErr error
	depth    int
}

func (q *fakeQueue) Claim(context.Context, int) ([]storage.Pending, error) {
	q.claims++
	if q.claimErr != nil {
		return nil, q.claimErr
	}
	if len(q.batches) == 0 {
		return nil, nil
	}
	b := q.batches[0]
	q.batches = q.batches[1:]
	return b, nil
}

func (q *fakeQueue) MarkSent(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.marked = append(q.marked, id)
	return nil
}

func (q *fakeQueue) Retry(ctx context.Context, id int64, attempts int, retryAfter time.Duration, reason string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	q.retries = append(q.retries, retryCall{id, attempts, retryAfter, reason})
	return q.gaveUpOn[id], nil
}

func (q *fakeQueue) Fail(ctx context.Context, id int64, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.failed = append(q.failed, id)
	return nil
}

func (q *fakeQueue) DropRecipient(ctx context.Context, userID int64, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.dropped = append(q.dropped, userID)
	return nil
}

func (q *fakeQueue) QueueDepth(context.Context) (int, time.Duration, error) {
	return q.depth, 0, nil
}

// --- helpers ----------------------------------------------------------------

func pending(id, userID, chatID int64, attempts int) storage.Pending {
	return storage.Pending{
		ID: id, UserID: userID, ChatID: chatID, Attempts: attempts,
		ReleaseID: 100 + id, ArtistName: "Radiohead", Title: "A Moon Shaped Pool",
		PrimaryType: "Album", ReleaseDate: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
		CoverURL: "https://coverartarchive.org/release/x/front-250",
		InfoURL:  "https://musicbrainz.org/artist/x",
	}
}

// testNotifier builds one with time and sleeping under the test's control, so
// rate limiting can be asserted without spending real seconds.
func testNotifier(q Queue, ch notify.Notifier) (*Notifier, *[]time.Duration, *time.Time) {
	n := New(q, notify.NewRegistry(ch), slog.New(slog.NewTextHandler(io.Discard, nil)))

	var slept []time.Duration
	clock := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	n.now = func() time.Time { return clock }
	n.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		clock = clock.Add(d)
		return nil
	}
	return n, &slept, &clock
}

// --- tests ------------------------------------------------------------------

func TestDeliverMarksSentAndPassesTheRelease(t *testing.T) {
	ch := &fakeChannel{}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(1, 10, 365932759, 1)
	n.deliver(context.Background(), &p)

	if len(q.marked) != 1 || q.marked[0] != 1 {
		t.Fatalf("marked = %v, want [1]", q.marked)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(ch.sent))
	}

	got := ch.sent[0]
	// The address is the numeric chat id as a string: a bot cannot address an
	// @username (SPEC.md C1).
	if got.to.Address != "365932759" {
		t.Fatalf("address = %q, want the numeric chat id", got.to.Address)
	}
	if got.rel.Title != "A Moon Shaped Pool" || got.rel.CoverURL == "" {
		t.Fatalf("release lost fields on the way to the channel: %+v", got.rel)
	}
}

// The most consequential branch in the project. A permanent failure means the
// chat is gone, so the subscriptions go with it — otherwise every future
// release rediscovers the same dead chat.
func TestPermanentFailureDropsTheRecipient(t *testing.T) {
	ch := &fakeChannel{err: &notify.DeliveryError{
		Disposition: notify.Permanent,
		Err:         errors.New("Forbidden: bot was blocked by the user"),
	}}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(1, 10, 999, 1)
	n.deliver(context.Background(), &p)

	if len(q.dropped) != 1 || q.dropped[0] != 10 {
		t.Fatalf("dropped = %v, want [10]", q.dropped)
	}
	if len(q.marked) != 0 || len(q.retries) != 0 || len(q.failed) != 0 {
		t.Fatalf("a permanent failure also marked/retried/failed: marked=%v retries=%d failed=%v",
			q.marked, len(q.retries), q.failed)
	}
}

// The mirror image, and the one that would be quietly destructive if confused
// with the above: a message we built wrong must cost one message, never
// somebody's subscriptions.
func TestBadMessageFailsOnlyThatNotification(t *testing.T) {
	ch := &fakeChannel{err: &notify.DeliveryError{
		Disposition: notify.BadMessage,
		Err:         errors.New("Bad Request: can't parse entities"),
	}}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(7, 10, 999, 1)
	n.deliver(context.Background(), &p)

	if len(q.failed) != 1 || q.failed[0] != 7 {
		t.Fatalf("failed = %v, want [7]", q.failed)
	}
	if len(q.dropped) != 0 {
		t.Fatal("a malformed message dropped the recipient's subscriptions")
	}
	if len(q.retries) != 0 {
		t.Fatal("a malformed message was scheduled for retry; it will fail identically")
	}
}

// Telegram's retry_after is not advice — ignoring it escalates to much longer
// lockouts, so it has to reach the queue verbatim.
func TestTransientFailureRetriesWithTheServersDelay(t *testing.T) {
	ch := &fakeChannel{err: &notify.DeliveryError{
		Disposition: notify.Transient,
		RetryAfter:  35 * time.Second,
		Err:         errors.New("Too Many Requests: retry after 35"),
	}}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(3, 10, 999, 2)
	n.deliver(context.Background(), &p)

	if len(q.retries) != 1 {
		t.Fatalf("retries = %d, want 1", len(q.retries))
	}
	r := q.retries[0]
	if r.id != 3 || r.attempts != 2 || r.retryAfter != 35*time.Second {
		t.Fatalf("retry = %+v, want id 3, attempts 2, retryAfter 35s", r)
	}
	if len(q.dropped) != 0 || len(q.failed) != 0 {
		t.Fatal("a transient failure dropped or failed instead of retrying")
	}
}

// An unrecognized error must behave like a transient one. Defaulting the other
// way would let an unfamiliar failure delete subscriptions.
func TestUnknownErrorIsTreatedAsTransient(t *testing.T) {
	ch := &fakeChannel{err: errors.New("something nobody has seen before")}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(4, 10, 999, 1)
	n.deliver(context.Background(), &p)

	if len(q.retries) != 1 {
		t.Fatalf("retries = %d, want 1", len(q.retries))
	}
	if len(q.dropped) != 0 {
		t.Fatal("an unrecognized error dropped the recipient")
	}
}

// Telegram allows about one message per second to a single chat, and somebody
// following five artists who all released on Friday gets five messages.
func TestPerChatGapIsEnforced(t *testing.T) {
	ch := &fakeChannel{}
	q := &fakeQueue{}
	n, slept, _ := testNotifier(q, ch)

	ctx := context.Background()
	first := pending(1, 10, 555, 1)
	second := pending(2, 10, 555, 1)

	n.deliver(ctx, &first)
	n.deliver(ctx, &second)

	// The first send has no predecessor, so nothing to wait for. The second
	// must wait out the remainder of the gap.
	if len(*slept) != 1 {
		t.Fatalf("slept %v, want exactly one wait before the second message", *slept)
	}
	if (*slept)[0] != perChatGap {
		t.Fatalf("waited %v before the second message, want %v", (*slept)[0], perChatGap)
	}
	if len(ch.sent) != 2 {
		t.Fatalf("sent %d, want 2", len(ch.sent))
	}
}

// Different chats do not wait for each other: the per-chat limit is per chat,
// and serializing everyone behind one recipient would make a Friday backlog
// take minutes for no reason.
func TestDifferentChatsDoNotWaitForEachOther(t *testing.T) {
	ch := &fakeChannel{}
	q := &fakeQueue{}
	n, slept, _ := testNotifier(q, ch)

	ctx := context.Background()
	for i, chatID := range []int64{111, 222, 333} {
		p := pending(int64(i+1), int64(i+10), chatID, 1)
		n.deliver(ctx, &p)
	}

	if len(*slept) != 0 {
		t.Fatalf("slept %v while sending to three different chats", *slept)
	}
	if len(ch.sent) != 3 {
		t.Fatalf("sent %d, want 3", len(ch.sent))
	}
}

// A send that succeeded but could not be recorded must not be retried as a
// send: the message is already in the user's chat.
func TestDeliveredButUnrecordedIsNotRetried(t *testing.T) {
	ch := &fakeChannel{}
	q := &failingMarkQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(1, 10, 999, 1)
	n.deliver(context.Background(), &p)

	if len(q.retries) != 0 || len(q.failed) != 0 {
		t.Fatalf("a delivered message was queued for another attempt: retries=%d failed=%v",
			len(q.retries), q.failed)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(ch.sent))
	}
}

type failingMarkQueue struct{ fakeQueue }

// SIGTERM while a message is on the wire. Telegram accepts it, so the outcome
// must still be written down: a MarkSent on the canceled context failed
// instantly, the row stayed leased, and the user got the release a second time
// five minutes after the restart.
func TestShutdownMidSendStillRecordsTheDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := &fakeChannel{during: func(context.Context) { cancel() }}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(1, 10, 999, 1)
	n.deliver(ctx, &p)

	if len(q.marked) != 1 || q.marked[0] != 1 {
		t.Fatalf("marked = %v after a send that completed during shutdown, want [1]", q.marked)
	}
}

// The same for every other outcome: a failure seen during shutdown must be
// recorded too, or the row comes back with its attempt uncounted.
func TestShutdownMidSendStillRecordsAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := &fakeChannel{
		during: func(context.Context) { cancel() },
		err:    errors.New("connection reset"),
	}
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, ch)

	p := pending(1, 10, 999, 1)
	n.deliver(ctx, &p)

	if len(q.retries) != 1 {
		t.Fatalf("retries = %d after a failed send during shutdown, want 1", len(q.retries))
	}
}

// A send has a deadline of its own, and shutdown does not cut it short. With
// no deadline one hung request stalled the only drain loop for good; cut short
// by shutdown, nobody could say whether the message had gone out.
func TestSendHasItsOwnDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var (
		deadline     time.Time
		hasDeadline  bool
		errAfterStop error
	)
	ch := &fakeChannel{during: func(c context.Context) {
		cancel() // shutdown arrives while the request is on the wire
		deadline, hasDeadline = c.Deadline()
		errAfterStop = c.Err()
	}}
	n, _, _ := testNotifier(&fakeQueue{}, ch)

	p := pending(1, 10, 999, 1)
	n.deliver(ctx, &p)

	if !hasDeadline {
		t.Fatal("the send had no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > sendTimeout {
		t.Fatalf("time to deadline = %v, want within (0, %v]", left, sendTimeout)
	}
	if errAfterStop != nil {
		t.Fatalf("shutdown canceled a send in flight: %v", errAfterStop)
	}
}

func (q *failingMarkQueue) MarkSent(context.Context, int64) error {
	return errors.New("database unreachable")
}

func TestDrainOnceHandlesAnEmptyQueue(t *testing.T) {
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, &fakeChannel{})

	sent, err := n.drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drainOnce on an empty queue: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sent = %d, want 0", sent)
	}
}

func TestDrainOncePropagatesClaimFailure(t *testing.T) {
	wantErr := errors.New("database on fire")
	q := &fakeQueue{claimErr: wantErr}
	n, _, _ := testNotifier(q, &fakeChannel{})

	if _, err := n.drainOnce(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the claim error", err)
	}
}

// A queue filling up with nobody able to drain it is the kind of thing that
// gets discovered weeks later, so it must be said out loud at startup.
func TestRunIdlesWithoutChannels(t *testing.T) {
	q := &fakeQueue{batches: [][]storage.Pending{{pending(1, 10, 999, 1)}}}
	n := New(q, notify.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := n.Run(ctx); err != nil {
		t.Fatalf("Run with no channels: %v", err)
	}
	if q.claims != 0 {
		t.Fatalf("claimed %d batches with nothing to deliver on", q.claims)
	}
}

// Shutdown is not a failure. Returning ctx.Err() here would make every SIGTERM
// look like a crashed worker to the errgroup.
func TestRunReturnsNilOnCancellation(t *testing.T) {
	q := &fakeQueue{}
	n, _, _ := testNotifier(q, &fakeChannel{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := n.Run(ctx); err != nil {
		t.Fatalf("Run after cancellation = %v, want nil", err)
	}
}

// --- batch behavior --------------------------------------------------------

// The per-item loop was previously untested: deliver() had tests, the loop
// around it did not.
func TestDrainOnceDeliversEveryRowInTheBatch(t *testing.T) {
	ch := &fakeChannel{}
	q := &fakeQueue{batches: [][]storage.Pending{{
		pending(1, 10, 111, 1),
		pending(2, 11, 222, 1),
		pending(3, 12, 333, 1),
	}}}
	n, _, _ := testNotifier(q, ch)

	sent, err := n.drainOnce(context.Background())
	if err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	if sent != 3 {
		t.Fatalf("sent = %d, want 3", sent)
	}
	if len(ch.sent) != 3 || len(q.marked) != 3 {
		t.Fatalf("channel got %d, queue marked %d, want 3 each", len(ch.sent), len(q.marked))
	}
}

// A 429 applies to the connection, not to one message. Continuing through the
// batch would earn one 429 per remaining row and escalate to a much longer
// lockout — the exact outcome honoring retry_after is meant to avoid.
func TestBatchIsAbandonedWhenTheServerAsksUsToWait(t *testing.T) {
	rateLimited := &notify.DeliveryError{
		Disposition: notify.Transient,
		RetryAfter:  30 * time.Second,
		Err:         errors.New("Too Many Requests: retry after 30"),
	}
	// First send succeeds, second is rate limited, and the third must never be
	// attempted at all.
	ch := &fakeChannel{errs: []error{nil, rateLimited, nil}}
	q := &fakeQueue{batches: [][]storage.Pending{{
		pending(1, 10, 111, 1),
		pending(2, 11, 222, 1),
		pending(3, 12, 333, 1),
	}}}
	n, _, _ := testNotifier(q, ch)

	if _, err := n.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	if len(ch.sent) != 2 {
		t.Fatalf("attempted %d sends, want 2 — the batch kept going after a 429", len(ch.sent))
	}
	if len(q.retries) != 1 || q.retries[0].id != 2 {
		t.Fatalf("retries = %+v, want the rate-limited row rescheduled", q.retries)
	}
	// The untouched row keeps its lease and comes back on its own; it must not
	// have been failed or dropped.
	if len(q.failed) != 0 || len(q.dropped) != 0 {
		t.Fatalf("abandoned rows were failed/dropped: failed=%v dropped=%v", q.failed, q.dropped)
	}
}

// A transient failure with no server-supplied delay is one bad send, not a rate
// limit. The batch must continue, or one flaky chat would stall everyone else.
func TestBatchContinuesAfterAPlainTransientFailure(t *testing.T) {
	ch := &fakeChannel{errs: []error{errors.New("connection reset"), nil, nil}}
	q := &fakeQueue{batches: [][]storage.Pending{{
		pending(1, 10, 111, 1),
		pending(2, 11, 222, 1),
		pending(3, 12, 333, 1),
	}}}
	n, _, _ := testNotifier(q, ch)

	if _, err := n.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	if len(ch.sent) != 3 {
		t.Fatalf("attempted %d sends, want 3 — one bad send stalled the batch", len(ch.sent))
	}
	if len(q.marked) != 2 {
		t.Fatalf("marked %d, want 2", len(q.marked))
	}
}

// Once a chat is known gone, the rest of that user's batch must not be
// attempted: those rows are already marked skipped, and sending anyway spends
// rate-limit budget to earn more errors.
func TestDroppedRecipientsRemainingRowsAreNotAttempted(t *testing.T) {
	gone := &notify.DeliveryError{
		Disposition: notify.Permanent,
		Err:         errors.New("Forbidden: bot was blocked by the user"),
	}
	ch := &fakeChannel{errs: []error{gone, nil, nil}}
	q := &fakeQueue{batches: [][]storage.Pending{{
		pending(1, 10, 111, 1), // user 10 — turns out to be gone
		pending(2, 10, 111, 1), // user 10 again — must be skipped
		pending(3, 11, 222, 1), // somebody else — must still be delivered
	}}}
	n, _, _ := testNotifier(q, ch)

	if _, err := n.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}

	if len(ch.sent) != 2 {
		t.Fatalf("attempted %d sends, want 2 — a dropped chat was messaged again", len(ch.sent))
	}
	if ch.sent[1].to.Address != "222" {
		t.Fatalf("second send went to %q, want the other user", ch.sent[1].to.Address)
	}
	if len(q.dropped) != 1 {
		t.Fatalf("dropped = %v, want one drop", q.dropped)
	}
	if len(q.marked) != 1 || q.marked[0] != 3 {
		t.Fatalf("marked = %v, want only the bystander's row", q.marked)
	}
}

// If the drop itself failed, the recipient is not actually known-gone, so the
// short-circuit must not engage — otherwise a database blip would silently
// swallow that user's remaining releases.
func TestDropFailureDoesNotSuppressTheRestOfTheBatch(t *testing.T) {
	gone := &notify.DeliveryError{
		Disposition: notify.Permanent,
		Err:         errors.New("Forbidden: bot was blocked by the user"),
	}
	ch := &fakeChannel{errs: []error{gone, gone}}
	q := &failingDropQueue{}
	q.batches = [][]storage.Pending{{
		pending(1, 10, 111, 1),
		pending(2, 10, 111, 1),
	}}
	n, _, _ := testNotifier(q, ch)

	if _, err := n.drainOnce(context.Background()); err != nil {
		t.Fatalf("drainOnce: %v", err)
	}
	if len(ch.sent) != 2 {
		t.Fatalf("attempted %d sends, want 2 — a failed drop was treated as done", len(ch.sent))
	}
}

type failingDropQueue struct{ fakeQueue }

func (q *failingDropQueue) DropRecipient(context.Context, int64, string) error {
	return errors.New("database unreachable")
}

// Per-chat timing state must not accumulate for the life of the process.
func TestIdleChatsAreForgotten(t *testing.T) {
	ch := &fakeChannel{}
	q := &fakeQueue{batches: [][]storage.Pending{
		{pending(1, 10, 111, 1)},
		{pending(2, 11, 222, 1)},
	}}
	n, _, clock := testNotifier(q, ch)

	ctx := context.Background()
	if _, err := n.drainOnce(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if len(n.lastSent) != 1 {
		t.Fatalf("lastSent holds %d entries after one send, want 1", len(n.lastSent))
	}

	// Move past the gap: chat 111 can no longer delay anything.
	*clock = clock.Add(10 * time.Second)
	if _, err := n.drainOnce(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if _, stillThere := n.lastSent[111]; stillThere {
		t.Fatal("an idle chat kept its timing entry; the map grows without bound")
	}
	if _, ok := n.lastSent[222]; !ok {
		t.Fatal("the chat just messaged was forgotten; the per-chat gap would not apply")
	}
}
