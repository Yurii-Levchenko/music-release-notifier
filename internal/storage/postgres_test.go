package storage_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// The session limits only exist if they reach the server, and DATABASE_URL
// must be able to override them for a one-off heavy job.
func TestOpenAppliesSessionLimits(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx, pool := openForTest(t, dsn)
	assertSetting(ctx, t, pool, "statement_timeout", "30s")
	assertSetting(ctx, t, pool, "lock_timeout", "5s")
	assertSetting(ctx, t, pool, "idle_in_transaction_session_timeout", "1min")

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	ctx, overridden := openForTest(t, dsn+sep+"statement_timeout=45s")
	assertSetting(ctx, t, overridden, "statement_timeout", "45s")
	assertSetting(ctx, t, overridden, "lock_timeout", "5s")
}

// Migrate lifts the limits for its own session. A connection that went back
// to the pool still lifted would serve the bot's queries with no timeout at
// all — the exact failure the limits exist to prevent, and an invisible one.
func TestMigrateRestoresSessionLimits(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx, pool := openForTest(t, dsn)
	if err := storage.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Hold every connection the pool has, so the one Migrate used is among
	// them whichever the pool would have handed out next.
	total := int(pool.Stat().TotalConns())
	if total == 0 {
		t.Fatal("pool has no connections after Migrate; the test would prove nothing")
	}
	for range total {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		t.Cleanup(conn.Release)
		assertSetting(ctx, t, conn, "statement_timeout", "30s")
		assertSetting(ctx, t, conn, "lock_timeout", "5s")
	}
}

func openForTest(t *testing.T, dsn string) (context.Context, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func assertSetting(ctx context.Context, t *testing.T, q rowQuerier, name, want string) {
	t.Helper()
	var got string
	if err := q.QueryRow(ctx, "SHOW "+name).Scan(&got); err != nil {
		t.Fatalf("SHOW %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
