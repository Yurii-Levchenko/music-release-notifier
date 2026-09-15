package telegram

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// A selection is which rows of one page of /list are ticked.
//
// It lives entirely inside callback_data. That is the whole design: a page
// holds 20 entries, 20 bits is five hex characters, and callback_data allows
// 64 bytes (SPEC C6). The alternative — a table of half-finished selections —
// would need a row per user, a cleanup job for the ones abandoned mid-tap, and
// would break every time the bot restarts. None of that buys anything a
// five-character number does not.
//
// Encoded as `pick:<page>:<mask>:<index>:<digest>`, where index is the row
// being toggled and digest guards the page against having changed underneath.
type selection struct {
	page int
	// mask is a bitmask over positions within the page, bit 0 being the first
	// row shown.
	mask uint32
	// digest fingerprints the page's artists. The mask addresses positions, not
	// artists, so if the page changed between drawing and confirming, those
	// positions now mean different people — and this is a destructive action.
	digest string
}

// selected reports whether the row at this position within the page is ticked.
func (s selection) selected(i int) bool {
	if i < 0 || i >= 32 {
		return false
	}
	return s.mask&(1<<uint(i)) != 0
}

// toggle returns the selection with one row flipped.
func (s selection) toggle(i int) selection {
	if i < 0 || i >= 32 {
		return s
	}
	s.mask ^= 1 << uint(i)
	return s
}

// count is how many rows are ticked.
func (s selection) count() int {
	n := 0
	for m := s.mask; m != 0; m &= m - 1 {
		n++
	}
	return n
}

// mbids resolves the ticked positions against the page as it is now.
//
// Returns ok=false when the page no longer matches the one the selection was
// drawn against. Refusing is the only safe answer: the mask addresses
// positions, and acting on stale positions unsubscribes the wrong artists.
func (s selection) mbids(items []storage.Subscription) (chosen, names []string, ok bool) {
	if s.digest != pageDigest(items) {
		return nil, nil, false
	}
	for i := range items {
		if s.selected(i) {
			chosen = append(chosen, items[i].MBID)
			names = append(names, items[i].Name)
		}
	}
	return chosen, names, true
}

// pageDigest fingerprints the artists on a page, in order.
//
// Short on purpose: it shares callback_data with everything else, and it is
// guarding against a page that shifted between two taps, not against a forged
// callback. Telegram only delivers callback_data the bot itself sent.
func pageDigest(items []storage.Subscription) string {
	h := fnv.New32a()
	for i := range items {
		_, _ = h.Write([]byte(items[i].MBID))
	}
	return fmt.Sprintf("%04x", h.Sum32()&0xffff)
}

// newSelection starts an empty selection for a page.
func newSelection(page int, items []storage.Subscription) selection {
	return selection{page: page, digest: pageDigest(items)}
}

// selectAll ticks every row that exists on the page.
func selectAll(page int, items []storage.Subscription) selection {
	s := newSelection(page, items)
	for i := range items {
		if i < 32 {
			s.mask |= 1 << uint(i)
		}
	}
	return s
}

// encode renders the selection into callback_data, with the row being acted on.
func (s selection) encode(prefix string, index int) string {
	return fmt.Sprintf("%s:%d:%x:%d:%s", prefix, s.page, s.mask, index, s.digest)
}

// decodeSelection parses what encode produced. index is -1 when the payload
// carries no row, which is how the apply and clear buttons look.
func decodeSelection(rest string) (s selection, index int, ok bool) {
	parts := strings.Split(rest, ":")
	if len(parts) != 4 {
		return selection{}, 0, false
	}

	page, err := strconv.Atoi(parts[0])
	if err != nil || page < 0 {
		return selection{}, 0, false
	}
	mask, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return selection{}, 0, false
	}
	index, err = strconv.Atoi(parts[2])
	if err != nil {
		return selection{}, 0, false
	}
	if parts[3] == "" {
		return selection{}, 0, false
	}

	return selection{page: page, mask: uint32(mask), digest: parts[3]}, index, true
}
