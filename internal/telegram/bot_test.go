package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
)

// loopOnlyBot goes through the real constructor with nil dependencies. The
// update loop touches none of them, and building the struct literally instead
// would let the test drift from NewBot — which it did: a new field defaulted in
// the constructor was nil here, and the loop panicked on the first update.
func loopOnlyBot() *Bot {
	return NewBot(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fakePoller stands in for telego's GetUpdates. Each call takes the next
// scripted response; once the script runs out, calls block until the test is
// over, the way an idle long poll would.
type fakePoller struct {
	mu        sync.Mutex
	responses []pollResponse
	params    []telego.GetUpdatesParams
	deadlines []time.Duration
	calls     chan struct{}
	release   chan struct{}
}

type pollResponse struct {
	updates []telego.Update
	err     error
}

func newFakePoller(responses ...pollResponse) *fakePoller {
	return &fakePoller{responses: responses, calls: make(chan struct{}, 64), release: make(chan struct{})}
}

func (f *fakePoller) getUpdates(ctx context.Context, params *telego.GetUpdatesParams) ([]telego.Update, error) {
	f.mu.Lock()
	f.params = append(f.params, *params)
	if d, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(d))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
	var r *pollResponse
	if len(f.responses) > 0 {
		r = &f.responses[0]
		f.responses = f.responses[1:]
	}
	f.mu.Unlock()
	f.calls <- struct{}{}

	if r != nil {
		return r.updates, r.err
	}
	// Ignores ctx on purpose: the fasthttp caller does too once the request is
	// on the wire, and the loop must not depend on it noticing.
	<-f.release
	return nil, nil
}

func (f *fakePoller) waitCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-f.calls:
		case <-deadline:
			t.Fatalf("getUpdates called %d times, want at least %d", i, n)
		}
	}
}

func runPoll(t *testing.T, b *Bot, f *fakePoller) (cancel func() error) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.poll(ctx, f.getUpdates) }()
	t.Cleanup(func() { close(f.release) })
	return func() error {
		stop()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("poll did not return after cancellation")
			return nil
		}
	}
}

// The failure the old loop could not see. telego retried a failing getUpdates
// behind a channel that stayed open while a ticker kept beating, so a revoked
// token or a dead connection read as a healthy, idle bot and the heartbeat
// stayed green. A poll that fails must not count as progress.
func TestFailingPollsDoNotReportProgress(t *testing.T) {
	var beats atomic.Int64
	b := loopOnlyBot()
	b.ReportProgressTo(func() { beats.Add(1) })

	unauthorized := errors.New("telego: getUpdates: api: 401 \"Unauthorized\"")
	f := newFakePoller(pollResponse{err: unauthorized}, pollResponse{err: unauthorized})
	stop := runPoll(t, b, f)

	f.waitCalls(t, 2) // the second call proves the loop retried rather than exited
	if got := beats.Load(); got != 0 {
		t.Fatalf("beats = %d while every getUpdates failed, want 0", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// An idle bot must still report progress: an empty, successful long poll is
// Telegram saying "nothing for you", and it is the only beat a quiet night has.
func TestEmptySuccessfulPollReportsProgress(t *testing.T) {
	var beats atomic.Int64
	b := loopOnlyBot()
	b.ReportProgressTo(func() { beats.Add(1) })

	f := newFakePoller(pollResponse{})
	stop := runPoll(t, b, f)

	f.waitCalls(t, 2)
	if got := beats.Load(); got != 1 {
		t.Fatalf("beats = %d after one empty successful poll, want 1", got)
	}
	if err := stop(); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// Updates are confirmed by the next call's offset. Getting it wrong either
// replays every update forever or skips the ones in between.
func TestOffsetAdvancesPastHandledUpdates(t *testing.T) {
	var beats atomic.Int64
	b := loopOnlyBot()
	b.ReportProgressTo(func() { beats.Add(1) })

	// Updates with no message, callback or member change: handle ignores them,
	// which is what makes them safe to use here without a full bot.
	f := newFakePoller(pollResponse{updates: []telego.Update{{UpdateID: 5}, {UpdateID: 6}}})
	stop := runPoll(t, b, f)

	f.waitCalls(t, 2)
	if err := stop(); err != nil {
		t.Fatalf("poll: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.params[0].Offset != 0 || f.params[1].Offset != 7 {
		t.Fatalf("offsets = %d, %d; want 0, then 7", f.params[0].Offset, f.params[1].Offset)
	}
	if got := beats.Load(); got != 3 { // the poll, then each update
		t.Fatalf("beats = %d, want 3", got)
	}
}

// Every call carries a deadline: the fasthttp caller honors a deadline and
// nothing else, so a call without one could hang on a dead connection forever.
func TestEveryPollHasADeadline(t *testing.T) {
	b := loopOnlyBot()
	f := newFakePoller(pollResponse{})
	stop := runPoll(t, b, f)

	f.waitCalls(t, 2)
	if err := stop(); err != nil {
		t.Fatalf("poll: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for i, d := range f.deadlines {
		if d <= 0 || d > pollDeadline {
			t.Fatalf("call %d: time to deadline = %v, want within (0, %v]", i, d, pollDeadline)
		}
	}
	if f.params[0].Timeout != int(pollTimeout/time.Second) {
		t.Fatalf("long-poll timeout = %d s, want %v", f.params[0].Timeout, pollTimeout)
	}
}

// Shutdown must not wait out a long poll that is already on the wire. The fake
// ignores cancellation, as the real caller does, so this passes only if the
// loop stops waiting for it.
func TestShutdownDoesNotWaitForAnInFlightPoll(t *testing.T) {
	b := loopOnlyBot()
	f := newFakePoller()
	stop := runPoll(t, b, f)

	f.waitCalls(t, 1)
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("clean shutdown reported as an error: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("shutdown took %v with a poll in flight", took)
	}
}

// A nil callback must be ignored rather than stored, or the first beat panics.
func TestReportProgressToIgnoresNil(t *testing.T) {
	b := loopOnlyBot()
	b.ReportProgressTo(nil)

	f := newFakePoller(pollResponse{})
	stop := runPoll(t, b, f)
	f.waitCalls(t, 2)
	if err := stop(); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// The welcome message has to describe the bot that exists. Its last line said
// "працює /start, решта — на підході" from S1 until now, telling everybody who
// pressed start that search, subscriptions and delivery did not work.
func TestHelpTextDescribesWhatWorks(t *testing.T) {
	got := helpText("music_release_radar_bot")

	for _, want := range []string{"/search", "/list", "/stop", "Підписатись"} {
		if !strings.Contains(got, want) {
			t.Errorf("help text does not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "працює /start") {
		t.Fatalf("the help text still claims only /start works:\n%s", got)
	}
}

// Plain text is how most people search, and it is invisible unless the help
// says so.
func TestHelpTextMentionsSearchingByTyping(t *testing.T) {
	got := helpText("bot")

	if !strings.Contains(got, "напиши ім'я") {
		t.Fatalf("typing a name is not mentioned as a way to search:\n%s", got)
	}
}

// The two things somebody would otherwise meet as a disappointment: a release
// can arrive a day late, and subscribing pays off immediately.
func TestHelpTextSetsExpectations(t *testing.T) {
	got := helpText("bot")

	if !strings.Contains(got, "раз на добу") {
		t.Errorf("the daily cadence is not stated:\n%s", got)
	}
	if !strings.Contains(got, "3 дні") {
		t.Errorf("the catch-up window is not stated:\n%s", got)
	}
}

// Before Init the bot does not know its own username, and the help must still
// read as a sentence.
func TestHelpTextWithoutAUsername(t *testing.T) {
	got := helpText("")

	if strings.Contains(got, "@ ") || strings.Contains(got, "@<") {
		t.Fatalf("a dangling @ with no username:\n%s", got)
	}
	if !strings.Contains(got, "Цей бот") {
		t.Fatalf("no fallback name:\n%s", got)
	}
}
