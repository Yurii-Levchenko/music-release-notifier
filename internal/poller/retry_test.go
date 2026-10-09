package poller

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A failed poll used to wait the full day. One ListenBrainz blip, or a laptop
// resuming before its network, made every release a day late and kept
// PollerStalled firing until the next attempt.
func TestFailedPollIsRetriedWithinTheHour(t *testing.T) {
	p := New(nil, nil, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := p.nextPollIn(errors.New("listenbrainz: 503")); got != time.Hour {
		t.Fatalf("after a failure: next poll in %v, want 1h", got)
	}
	if got := p.nextPollIn(nil); got != 24*time.Hour {
		t.Fatalf("after a success: next poll in %v, want the 24h interval", got)
	}
}

// An interval shorter than the retry must not be stretched by a failure.
func TestRetryNeverExceedsTheInterval(t *testing.T) {
	p := New(nil, nil, 10*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := p.nextPollIn(errors.New("boom")); got != 10*time.Minute {
		t.Fatalf("next poll in %v, want the 10m interval", got)
	}
}
