package telegram

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

// The failure this whole health story exists for. When telego's long polling
// dies it closes the channel; returning nil there looks like a clean exit to
// the errgroup, so the process keeps running with no bot and nobody finds out.
// That is the shape of the 14-hour silence on 29.08.2026.
func TestPollingDyingIsAnError(t *testing.T) {
	b := loopOnlyBot()
	updates := make(chan telego.Update)
	close(updates)

	err := b.consume(context.Background(), updates)
	if err == nil {
		t.Fatal("long polling died and consume reported a clean exit; " +
			"the process would run on with a dead bot")
	}
}

// The same channel closure during shutdown is expected and must not be
// reported as a failure, or every deploy would look like a crash.
func TestChannelClosureDuringShutdownIsClean(t *testing.T) {
	b := loopOnlyBot()
	updates := make(chan telego.Update)
	close(updates)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.consume(ctx, updates); err != nil {
		t.Fatalf("clean shutdown reported as an error: %v", err)
	}
}

// An idle bot must still report progress. Long polling delivers nothing when
// nobody is typing, so a traffic-driven heartbeat would mark a perfectly
// healthy bot as wedged after five quiet minutes.
func TestIdleBotStillReportsProgress(t *testing.T) {
	var beats atomic.Int64
	b := loopOnlyBot()
	b.ReportProgressTo(func() { beats.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan telego.Update)

	done := make(chan error, 1)
	go func() { done <- b.consume(ctx, updates) }()

	// The startup beat happens before any traffic, which is the property that
	// matters: the registry is not empty while the bot waits.
	deadline := time.After(2 * time.Second)
	for beats.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("an idle bot never reported progress")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("consume: %v", err)
	}
}

func TestHandledUpdateReportsProgress(t *testing.T) {
	var beats atomic.Int64
	b := loopOnlyBot()
	b.ReportProgressTo(func() { beats.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	updates := make(chan telego.Update, 1)
	// An update with no message, callback or member change: handle ignores it,
	// which is what makes it safe to use here without a full bot.
	updates <- telego.Update{UpdateID: 1}

	done := make(chan error, 1)
	go func() { done <- b.consume(ctx, updates) }()

	deadline := time.After(2 * time.Second)
	for beats.Load() < 2 { // one at startup, one for the update
		select {
		case <-deadline:
			cancel()
			t.Fatalf("beats = %d after handling an update, want at least 2", beats.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("consume: %v", err)
	}
}

// A nil callback must be ignored rather than stored, or the first beat panics.
func TestReportProgressToIgnoresNil(t *testing.T) {
	b := loopOnlyBot()
	b.ReportProgressTo(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	updates := make(chan telego.Update)

	if err := b.consume(ctx, updates); err != nil {
		t.Fatalf("consume: %v", err)
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
