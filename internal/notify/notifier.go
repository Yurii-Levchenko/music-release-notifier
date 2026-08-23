// Package notify is the boundary between the domain and any delivery channel.
//
// This package exists from the first commit on purpose (SPEC.md D13). Nothing
// here may mention Telegram, chat ids, parse modes, or retry_after: the domain
// says "deliver this release to this recipient" and knows nothing else. When a
// second channel arrives, it implements Notifier and no domain code changes.
//
// The rule to hold: if you find yourself importing a Telegram type into a
// caller of this package, the abstraction has already leaked.
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Release is the domain's view of something worth telling someone about.
type Release struct {
	ID          int64
	ArtistName  string
	Title       string
	PrimaryType string // Album | Single | EP
	ReleaseDate time.Time
	// CoverURL is empty for roughly a quarter of releases (SPEC C27b), so every
	// channel needs a no-image path. Not a rare edge case.
	CoverURL string
	InfoURL  string
}

// Recipient says where to deliver without saying how. Address is opaque to the
// domain: a chat id for Telegram, a mailbox for email.
type Recipient struct {
	UserID  int64
	Kind    string
	Address string
}

// Notifier delivers one release to one recipient.
type Notifier interface {
	// Kind matches channels.kind in the database.
	Kind() string
	// Send returns nil on delivery. On failure it should return a *DeliveryError
	// so the outbox can decide retry-vs-drop without inspecting channel errors.
	Send(ctx context.Context, to Recipient, rel Release) error
}

// Disposition tells the outbox what to do about a failure. This is the entire
// vocabulary the notifier worker needs; the mapping from a channel's own error
// codes lives inside that channel's package (for Telegram, see SPEC.md §10).
type Disposition int

const (
	// Transient — try again later. Network blips, 5xx, rate limits.
	Transient Disposition = iota
	// Permanent — this recipient can never be reached again: blocked the bot,
	// deleted their account, unknown chat. Drop the subscription.
	Permanent
	// BadMessage — our bug, not their fault. Mark this one message failed and
	// leave the subscription alone. A malformed caption must not cost a user
	// their subscriptions.
	BadMessage
)

func (d Disposition) String() string {
	switch d {
	case Transient:
		return "transient"
	case Permanent:
		return "permanent"
	case BadMessage:
		return "bad_message"
	default:
		return fmt.Sprintf("disposition(%d)", int(d))
	}
}

// DeliveryError wraps a channel failure with the only two things the outbox
// cares about: what to do, and how long to wait.
type DeliveryError struct {
	Disposition Disposition
	// RetryAfter is honoured verbatim when the channel supplies one. Telegram
	// does, in seconds, and ignoring it escalates to longer lockouts.
	RetryAfter time.Duration
	Err        error
}

func (e *DeliveryError) Error() string {
	if e.Err == nil {
		return "delivery failed: " + e.Disposition.String()
	}
	return fmt.Sprintf("delivery failed (%s): %v", e.Disposition, e.Err)
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// DispositionOf reports how the outbox should treat err. Anything that is not a
// *DeliveryError is treated as Transient: an unrecognised failure must never
// silently delete someone's subscriptions.
func DispositionOf(err error) (Disposition, time.Duration) {
	var de *DeliveryError
	if errors.As(err, &de) {
		return de.Disposition, de.RetryAfter
	}
	return Transient, 0
}

// Registry resolves a channel kind to its implementation.
type Registry struct {
	byKind map[string]Notifier
}

func NewRegistry(notifiers ...Notifier) *Registry {
	r := &Registry{byKind: make(map[string]Notifier, len(notifiers))}
	for _, n := range notifiers {
		r.byKind[n.Kind()] = n
	}
	return r
}

// ErrNoNotifier means the database holds a channel kind this build cannot
// deliver to — e.g. an 'email' row while only Telegram is compiled in.
var ErrNoNotifier = errors.New("no notifier registered for channel kind")

func (r *Registry) For(kind string) (Notifier, error) {
	n, ok := r.byKind[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoNotifier, kind)
	}
	return n, nil
}

func (r *Registry) Kinds() []string {
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	return out
}
