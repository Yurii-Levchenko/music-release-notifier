// Package health tracks whether the workers are actually doing their job, and
// reports that to an external dead-man's switch.
//
// The reason this package exists is a real incident (SPEC §15a): on 29.08.2026
// the container failed to come up after a host reboot and the bot was silent
// for about fourteen hours. Nothing noticed, because nothing outside the
// process was watching.
//
// The subtle part is what "watching" has to mean. A goroutine that pings a
// monitoring URL on a timer proves that the timer works — not that the product
// works. The bot's long polling can be wedged, the notifier can be stuck on a
// dead connection, and a timer will happily keep reporting success. So the ping
// is gated: each worker reports progress, and the heartbeat is only sent when
// every worker has made progress within the time it is allowed to be quiet.
//
// Silence is therefore the alarm. If this process is dead, or wedged, or cannot
// reach its database, no ping goes out and the external monitor reports it.
package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Component is one thing that has to keep working for the product to work.
type Component struct {
	name string
	// budget is how long this component may go quiet before something is
	// wrong. It is per component because the workers have wildly different
	// rhythms: the poller runs once a day by design, the bot every minute.
	budget time.Duration
	// lastBeat holds unix nanoseconds. Atomic rather than mutex-guarded
	// because it is written on every worker cycle and read by the heartbeat.
	lastBeat atomic.Int64
	// now comes from the registry rather than from the time package. Reading
	// the clock directly here made the component untestable while looking
	// correct: the registry's clock could be replaced and Beat would quietly
	// ignore it. Set once at construction, so no synchronization is needed.
	now func() time.Time
}

// Beat records that this component just did its job.
func (c *Component) Beat() {
	c.lastBeat.Store(c.now().UnixNano())
}

func (c *Component) last() time.Time {
	return time.Unix(0, c.lastBeat.Load())
}

// Status is one component's state at a moment in time.
type Status struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	// QuietFor and Budget are empty for a probe, which has no rhythm to be
	// quiet against. Rendering "quiet n/a, budget n/a" in an alert was worse
	// than saying nothing: it fills the line a human reads at 3am with two
	// fields that carry no information.
	QuietFor string `json:"quiet_for,omitempty"`
	Budget   string `json:"budget,omitempty"`
}

// Probe is a check that has to be performed rather than reported — reaching the
// database, for instance. Unlike a component, nobody beats for it.
type Probe struct {
	Name  string
	Check func(context.Context) error
}

// Registry is the set of components and probes that define "working".
type Registry struct {
	mu         sync.RWMutex
	components []*Component
	probes     []Probe

	now func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{now: time.Now}
}

// Register adds a component. The initial beat is set to registration time, so a
// worker that legitimately has nothing to do yet is not immediately reported as
// broken — the budget runs from process start.
func (r *Registry) Register(name string, budget time.Duration) *Component {
	c := &Component{name: name, budget: budget, now: r.now}
	c.lastBeat.Store(r.now().UnixNano())

	r.mu.Lock()
	defer r.mu.Unlock()
	r.components = append(r.components, c)
	return c
}

// AddProbe registers a check that is performed at report time.
func (r *Registry) AddProbe(name string, check func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, Probe{Name: name, Check: check})
}

// Report evaluates everything. ok is false if any component is over budget or
// any probe fails.
func (r *Registry) Report(ctx context.Context) (ok bool, statuses []Status) {
	r.mu.RLock()
	components := make([]*Component, len(r.components))
	copy(components, r.components)
	probes := make([]Probe, len(r.probes))
	copy(probes, r.probes)
	r.mu.RUnlock()

	now := r.now()
	ok = true

	for _, c := range components {
		quiet := now.Sub(c.last())
		healthy := quiet <= c.budget
		if !healthy {
			ok = false
		}
		statuses = append(statuses, Status{
			Name:     c.name,
			Healthy:  healthy,
			QuietFor: quiet.Round(time.Second).String(),
			Budget:   c.budget.String(),
		})
	}

	for _, p := range probes {
		err := p.Check(ctx)
		if err != nil {
			ok = false
		}
		statuses = append(statuses, Status{Name: p.Name, Healthy: err == nil})
	}

	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return ok, statuses
}

// Unhealthy renders the failing parts for a log line.
func Unhealthy(statuses []Status) string {
	var parts []string
	for _, s := range statuses {
		if !s.Healthy {
			if s.Budget == "" {
				parts = append(parts, s.Name)
				continue
			}
			parts = append(parts, fmt.Sprintf("%s (quiet %s, budget %s)", s.Name, s.QuietFor, s.Budget))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}
