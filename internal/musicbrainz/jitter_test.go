package musicbrainz

import (
	"testing"
	"time"
)

// Jitter must stay inside +/-25% and must actually vary, or it is decoration.
func TestJitterSpreadsWithinBounds(t *testing.T) {
	const base = 4 * time.Second
	lo, hi := base-base/4, base+base/4

	seen := map[time.Duration]bool{}
	for range 200 {
		got := jitter(base)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, outside [%v, %v]", base, got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 10 {
		t.Fatalf("jitter produced only %d distinct values; it is not spreading anything", len(seen))
	}
}

func TestJitterHandlesZero(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != -time.Second {
		t.Fatalf("jitter(negative) = %v, want it unchanged", got)
	}
}
