// Package httpx holds the HTTP surface. In S0 that is only /healthz; the v2
// extension API (SPEC §7) will be added here in S8.
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/health"
)

type Server struct {
	pool    *pgxpool.Pool
	health  *health.Registry
	metrics prometheus.Gatherer
	log     *slog.Logger
}

func New(pool *pgxpool.Pool, registry *health.Registry, gatherer prometheus.Gatherer, log *slog.Logger) *Server {
	return &Server{pool: pool, health: registry, metrics: gatherer, log: log}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /status", s.status)

	// /metrics is deliberately not on the public internet in the deployment:
	// it is scraped over the compose network. Exposing it would hand out chat
	// volumes and queue depths to anyone who asked.
	if s.metrics != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics, promhttp.HandlerOpts{
			// A collector that fails must not take the whole scrape with it:
			// one unreadable queue should not blind every other metric.
			ErrorHandling: promhttp.ContinueOnError,
		}))
	}
	return s.withLogging(mux)
}

type healthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// healthz reports readiness, which means the database is actually reachable —
// not merely that the process is alive. A health check that cannot fail is
// worth nothing to a restart policy.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	resp := healthResponse{Status: "ok", DB: "ok"}
	code := http.StatusOK

	if err := s.pool.Ping(ctx); err != nil {
		s.log.Error("healthz: database unreachable", "err", err)
		resp = healthResponse{Status: "degraded", DB: "unreachable"}
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

type statusResponse struct {
	Status     string          `json:"status"`
	Components []health.Status `json:"components"`
}

// status reports what each worker is actually doing, which /healthz cannot.
//
// The two endpoints answer different questions on purpose. /healthz is
// liveness for the container: can this process serve and reach its database.
// Wiring worker staleness into it would make Docker restart the process
// because the poller has nothing to do — a restart loop caused by a healthy
// idle worker. /status is the diagnostic view, and it is what a human opens
// after the dead-man's switch has already told them something is wrong.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	code := http.StatusOK
	resp := statusResponse{Status: "ok"}

	if s.health != nil {
		ok, statuses := s.health.Report(ctx)
		resp.Components = statuses
		if !ok {
			resp.Status = "degraded"
			code = http.StatusServiceUnavailable
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}
