package poller

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// A failed poll used to wait the full day. One ListenBrainz blip, or a laptop
// resuming before its network, made every release a day late and kept
// PollerStalled firing until the next attempt. Now: five minutes, doubling,
// capped at an hour; and the full interval again once a poll succeeds.
func TestFailedPollsBackOffFromFiveMinutesToAnHour(t *testing.T) {
	p := New(nil, nil, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	want := []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute,
		time.Hour, time.Hour,
	}
	for i, w := range want {
		if got := p.nextPollIn(i + 1); got != w {
			t.Errorf("after %d failures: next poll in %v, want %v", i+1, got, w)
		}
	}
	if got := p.nextPollIn(0); got != 24*time.Hour {
		t.Fatalf("after a success: next poll in %v, want the 24h interval", got)
	}
}

// An interval shorter than the retry must not be stretched by a failure.
func TestRetryNeverExceedsTheInterval(t *testing.T) {
	p := New(nil, nil, 3*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := p.nextPollIn(1); got != 3*time.Minute {
		t.Fatalf("next poll in %v, want the 3m interval", got)
	}
}
