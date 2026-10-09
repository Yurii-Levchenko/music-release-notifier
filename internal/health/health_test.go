package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A component that has just been registered must not read as broken. The poller
// runs once a day by design, and a process that reported itself unhealthy for
// the first 24 hours of every restart would be worse than no monitoring.
func TestFreshlyRegisteredComponentIsHealthy(t *testing.T) {
	r := NewRegistry()
	r.Register("poller", 25*time.Hour)

	ok, statuses := r.Report(context.Background())
	if !ok {
		t.Fatalf("a just-registered component is unhealthy: %s", Unhealthy(statuses))
	}
}

func TestComponentGoesUnhealthyPastItsBudget(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	bot := r.Register("bot", 5*time.Minute)

	// Beat, then let more than the budget elapse.
	bot.Beat()
	now = now.Add(6 * time.Minute)

	ok, statuses := r.Report(context.Background())
	if ok {
		t.Fatal("a component quiet past its budget reported healthy")
	}
	if Unhealthy(statuses) == "" {
		t.Fatal("nothing named as unhealthy; the alert would say nothing useful")
	}
}

// Budgets are per component precisely because the workers have different
// rhythms. A single global budget would either alert on the idle poller or
// never notice a wedged bot.
func TestBudgetsAreIndependent(t *testing.T) {
	r := NewRegistry()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	bot := r.Register("bot", 5*time.Minute)
	poller := r.Register("poller", 25*time.Hour)
	bot.Beat()
	poller.Beat()

	now = now.Add(10 * time.Minute)

	ok, statuses := r.Report(context.Background())
	if ok {
		t.Fatal("the wedged bot went unnoticed")
	}
	for _, s := range statuses {
		if s.Name == "poller" && !s.Healthy {
			t.Fatal("the poller was reported unhealthy 10 minutes into a daily cycle")
		}
	}
}

func TestFailingProbeMakesTheReportUnhealthy(t *testing.T) {
	r := NewRegistry()
	r.AddProbe("database", func(context.Context) error {
		return errors.New("connection refused")
	})

	ok, statuses := r.Report(context.Background())
	if ok {
		t.Fatal("an unreachable database reported healthy")
	}
	if Unhealthy(statuses) == "" {
		t.Fatal("the failing probe was not named")
	}
}

// --- the heartbeat ----------------------------------------------------------

// The whole mechanism. A ping while a worker is broken would tell the monitor
// that everything is fine, which is worse than having no monitor at all.
func TestHeartbeatWithheldWhenAWorkerIsUnhealthy(t *testing.T) {
	var pings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	r := NewRegistry()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	bot := r.Register("bot", time.Minute)
	bot.Beat()
	now = now.Add(2 * time.Minute)

	hb := NewHeartbeat(server.URL, time.Minute, r, quietLogger())
	hb.beatOnce(context.Background())

	if got := pings.Load(); got != 0 {
		t.Fatalf("sent %d pings while a worker was wedged, want 0", got)
	}
}

func TestHeartbeatSentWhenEverythingIsHealthy(t *testing.T) {
	var pings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	r := NewRegistry()
	r.Register("bot", time.Minute).Beat()
	r.AddProbe("database", func(context.Context) error { return nil })

	hb := NewHeartbeat(server.URL, time.Minute, r, quietLogger())
	hb.beatOnce(context.Background())

	if got := pings.Load(); got != 1 {
		t.Fatalf("sent %d pings while healthy, want 1", got)
	}
}

// A monitoring endpoint that is down must not take the process with it, and
// must not be mistaken for a delivered ping — from the outside a failed ping is
// indistinguishable from a dead process, so it has to be reported as a failure.
func TestHeartbeatSurvivesAnUnreachableEndpoint(t *testing.T) {
	r := NewRegistry()
	r.Register("bot", time.Minute).Beat()

	hb := NewHeartbeat("http://127.0.0.1:1/nope", time.Minute, r, quietLogger())

	if err := hb.ping(context.Background()); err == nil {
		t.Fatal("an unreachable endpoint was treated as a delivered ping")
	}
	hb.beatOnce(context.Background()) // and the loop must survive it
}

func TestHeartbeatTreatsAnErrorStatusAsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	r := NewRegistry()
	r.Register("bot", time.Minute).Beat()

	hb := NewHeartbeat(server.URL, time.Minute, r, quietLogger())
	if err := hb.ping(context.Background()); err == nil {
		t.Fatal("a 500 from the monitoring endpoint was treated as a delivered ping")
	}
}

// With no URL the process must say so rather than appear monitored.
func TestHeartbeatWithNoURLStopsCleanly(t *testing.T) {
	r := NewRegistry()
	hb := NewHeartbeat("", time.Minute, r, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := hb.Run(ctx); err != nil {
		t.Fatalf("Run with no URL = %v, want nil", err)
	}
}

// Shutdown must not ping. A clean stop that reassures the monitor would hide
// exactly the case the monitor exists for: the process is not running.
func TestShutdownDoesNotPing(t *testing.T) {
	var pings atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	r := NewRegistry()
	r.Register("bot", time.Minute).Beat()

	// An interval long enough that the only ping can be the initial one.
	hb := NewHeartbeat(server.URL, time.Hour, r, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := hb.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := pings.Load(); got != 1 {
		t.Fatalf("sent %d pings, want exactly the one at startup", got)
	}
}

// The ping URL is the secret: whoever holds it can keep the monitor quiet while
// the process is down. *url.Error prints the whole URL, so a plain dial failure
// used to copy it into the log — and from there into Loki for 30 days.
func TestPingFailureLogsWithoutTheURL(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	r := NewRegistry()
	r.Register("bot", time.Minute).Beat()

	const secret = "SECRET-PING-TOKEN-7f3a"
	hb := NewHeartbeat("http://127.0.0.1:1/"+secret, time.Minute, r, log)
	hb.beatOnce(context.Background())

	out := buf.String()
	if !strings.Contains(out, "heartbeat ping failed") {
		t.Fatalf("expected the failure to be logged, got:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("the ping URL leaked into the log:\n%s", out)
	}
}

func TestWithoutURLUnwrapsOnlyURLErrors(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	wrapped := &url.Error{Op: "Get", URL: "https://hc-ping.com/secret", Err: inner}
	// errors.Is alone would pass on the wrapped error too, since *url.Error
	// unwraps to inner; what matters is that the URL is gone from the text.
	got := withoutURL(wrapped)
	if !errors.Is(got, inner) || strings.Contains(got.Error(), "hc-ping.com") {
		t.Fatalf("withoutURL(*url.Error) = %q, want the cause without the URL", got)
	}
	plain := errors.New("heartbeat endpoint returned Not Found")
	if got := withoutURL(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Fatalf("withoutURL changed a non-URL error: %v", got)
	}
}
