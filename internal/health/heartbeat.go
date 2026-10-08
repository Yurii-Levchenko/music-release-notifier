package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const (
	// DefaultInterval is how often a healthy process checks in. The external
	// monitor is configured to expect a ping within a grace period roughly
	// twice this, so one missed ping is tolerated and two are not.
	DefaultInterval = 5 * time.Minute

	// pingTimeout is short on purpose. A slow monitoring endpoint must never
	// hold up the process, and a ping that arrives late is worth nothing.
	pingTimeout = 10 * time.Second
)

// Heartbeat pings an external dead-man's switch while the workers are healthy,
// and stays silent when they are not.
//
// Staying silent is the entire mechanism. There is no "report a problem" call,
// because a process that has died cannot make one — which is exactly how the
// fourteen-hour outage went unnoticed.
type Heartbeat struct {
	url      string
	interval time.Duration
	registry *Registry
	client   *http.Client
	log      *slog.Logger
}

func NewHeartbeat(pingURL string, interval time.Duration, registry *Registry, log *slog.Logger) *Heartbeat {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Heartbeat{
		url:      pingURL,
		interval: interval,
		registry: registry,
		client:   &http.Client{Timeout: pingTimeout},
		log:      log,
	}
}

// Run pings until ctx is canceled. With no URL configured it reports that
// clearly and stops: silently doing nothing would leave the deployment
// believing it is monitored when it is not.
func (h *Heartbeat) Run(ctx context.Context) error {
	if h.url == "" {
		h.log.Warn("heartbeat disabled: HEARTBEAT_URL is not set — " +
			"nothing outside this process will notice if it dies")
		<-ctx.Done()
		return nil
	}

	// String, not the Duration: slog renders a bare Duration as a nanosecond
	// integer, and "60000000000" is not what anyone wants to read in the line
	// that explains their monitoring.
	h.log.Info("heartbeat started", "interval", h.interval.String())

	// Ping immediately so a restart shows up as recovered without waiting out
	// a full interval.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Deliberately no farewell ping. A clean shutdown that silences the
			// monitor is correct: the process is no longer running, and that is
			// exactly what the monitor should be telling someone.
			h.log.Info("heartbeat stopping")
			return nil
		case <-timer.C:
		}

		h.beatOnce(ctx)
		timer.Reset(h.interval)
	}
}

func (h *Heartbeat) beatOnce(ctx context.Context) {
	ok, statuses := h.registry.Report(ctx)
	if !ok {
		// The whole point: withhold the ping so the monitor alerts. Logged at
		// error level because if the logs are reachable, this is the line that
		// explains what the alert is about.
		h.log.Error("withholding heartbeat: a worker is not healthy",
			"unhealthy", Unhealthy(statuses))
		return
	}

	if err := h.ping(ctx); err != nil {
		// A failed ping looks identical to a dead process from the outside, so
		// there is nothing to do but say so. It resolves itself on the next
		// interval if the network was the problem.
		//
		// The URL is stripped first. *url.Error prints the full request URL,
		// and the ping URL is the secret here: whoever holds it can keep the
		// monitor quiet while the process is down. Any network blip would
		// otherwise copy it into stdout, and from there into Loki for 30 days
		// (review 16.09, Security #6).
		h.log.Warn("heartbeat ping failed", "err", withoutURL(err))
		return
	}
	h.log.Debug("heartbeat sent")
}

func (h *Heartbeat) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, http.NoBody)
	if err != nil {
		return err
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &pingError{code: resp.StatusCode}
	}
	return nil
}

type pingError struct{ code int }

func (e *pingError) Error() string {
	return "heartbeat endpoint returned " + http.StatusText(e.code)
}

// withoutURL unwraps a *url.Error to its cause so the ping URL never reaches a
// log line. Anything else passes through unchanged.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
