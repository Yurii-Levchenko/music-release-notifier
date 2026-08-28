package storage

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// advisoryLockKey is an arbitrary constant; it only has to be stable and
// unlikely to collide with another application sharing the database.
const advisoryLockKey int64 = 8_23_1990

// Migrate applies every unapplied migration in lexical filename order, inside
// one transaction per migration.
//
// Hand-rolled rather than golang-migrate: the whole contract is "run these
// files once, in order", and 80 obvious lines beat a dependency plus its CLI.
// A session advisory lock makes it safe when several app instances boot at once.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// Serialize migration across instances. Released when the session ends,
	// but we unlock explicitly so a long-lived pooled connection is not held.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey); err != nil {
			log.Warn("release advisory lock", "err", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT        PRIMARY KEY,
			checksum    TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn.Conn())
	if err != nil {
		return err
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}

	pending := 0
	for _, name := range files {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		if prev, ok := applied[name]; ok {
			// A changed file that already ran means the repo and the database
			// disagree about history. Refuse rather than guess.
			if prev != checksum {
				return fmt.Errorf(
					"migration %s was already applied but its contents changed "+
						"(recorded %s, found %s): add a new migration instead of editing this one",
					name, prev[:12], checksum[:12])
			}
			continue
		}

		if err := applyOne(ctx, conn.Conn(), name, string(body), checksum); err != nil {
			return err
		}
		log.Info("migration applied", "version", name)
		pending++
	}

	if pending == 0 {
		log.Info("schema up to date", "migrations", len(files))
	} else {
		log.Info("migrations complete", "applied", pending, "total", len(files))
	}
	return nil
}

func applyOne(ctx context.Context, conn *pgx.Conn, name, body, checksum string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", name, err)
	}
	// Rollback is a no-op once the commit succeeds.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, body); err != nil {
		return fmt.Errorf("exec %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)`,
		name, checksum); err != nil {
		return fmt.Errorf("record %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

func migrationFiles() ([]string, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Filenames are zero-padded (0001_, 0002_), so lexical order is numeric order.
	sort.Strings(names)
	return names, nil
}
