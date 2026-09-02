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
}

func (f *fakeChannel) Kind() string { return "telegram" }

func (f *fakeChannel) Send(_ context.Context, to notify.Recipient, rel notify.Release) error {
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

func (q *fakeQueue) MarkSent(_ context.Context, id int64) error {
	q.marked = append(q.marked, id)
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, id int64, attempts int, retryAfter time.Duration, reason string) (bool, error) {
	q.retries = append(q.retries, retryCall{id, attempts, retryAfter, reason})
	return q.gaveUpOn[id], nil
}

func (q *fakeQueue) Fail(_ context.Context, id int64, _ string) error {
	q.failed = append(q.failed, id)
	return nil
}

func (q *fakeQueue) DropRecipient(_ context.Context, userID int64, _ string) error {
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
