// Package storage owns the database connection and the schema.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open builds a connection pool and verifies it can actually reach Postgres.
// pgxpool.New is lazy, so without the ping a broken DATABASE_URL would only
// surface on the first query — long after startup looked successful.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// Sized for a single small VPS (NFR-6), not for throughput.
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	applySessionLimits(cfg)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// sessionLimits bound how long the database can keep this app waiting
// (review 16.09, Data #8). Before them every limit was the server default —
// none — so one wedged statement stalled the bot or the notifier until a health
// budget noticed minutes later, and nothing said why.
//
// Every query this app runs is a short indexed read or a single-row write;
// thirty seconds is two orders of magnitude above the slowest. lock_timeout is
// shorter because waiting on a lock is never progress here: Claim skips locked
// rows by design. idle_in_transaction_session_timeout catches the bug of a
// transaction left open across network I/O, which would otherwise hold its
// locks until a restart.
var sessionLimits = map[string]string{
	"statement_timeout":                   "30s",
	"lock_timeout":                        "5s",
	"idle_in_transaction_session_timeout": "60s",
}

// applySessionLimits sets the limits as connection parameters, so they hold on
// every pooled connection and only for this app — psql and the backup's
// pg_dump keep the server defaults, which a full dump needs.
//
// A value already in DATABASE_URL wins. That leaves a way to run a one-off
// heavy job with a longer limit without a rebuild.
func applySessionLimits(cfg *pgxpool.Config) {
	if cfg.ConnConfig.ConnectTimeout == 0 {
		// A database that does not answer the handshake in five seconds is not
		// going to; failing fast lets the caller's retry or the health check act.
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	}
	for name, value := range sessionLimits {
		if _, set := cfg.ConnConfig.RuntimeParams[name]; !set {
			cfg.ConnConfig.RuntimeParams[name] = value
		}
	}
}
