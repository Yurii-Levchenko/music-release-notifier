package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
	"github.com/jackc/pgx/v5"
)

// These tests assert the invariants that the whole deduplication design rests
// on (SPEC.md NFR-1, NFR-2, D7). They deliberately test the *database*, not Go
// code: correctness lives in the constraints, so that is what must be covered.
//
// Each test runs inside a transaction that is always rolled back, so the tests
// leave no rows behind and can run against a dev database.
//
// Needs a live Postgres:  docker compose up -d db
// Skipped when TEST_DATABASE_URL is unset, so `go test ./...` stays green
// without Docker.
func testPool(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database invariant tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	return ctx, tx
}

// fixtures inserts one user, one artist and one release, returning their ids.
func fixtures(ctx context.Context, t *testing.T, tx pgx.Tx) (userID, releaseID int64, artistMBID string) {
	t.Helper()
	artistMBID = "a1b2c3d4-0000-0000-0000-0000000000ff"

	if err := tx.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES (987654321) RETURNING id`).
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO artists (mbid, name) VALUES ($1, 'Test Artist')`, artistMBID); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO releases
			(release_group_mbid, artist_mbid, title, primary_type, release_date, dedup_key)
		 VALUES ('b1b2c3d4-0000-0000-0000-0000000000ff', $1, 'Test Album', 'Album',
		         '2026-08-20', 'test|test album|2026-08-20')
		 RETURNING id`, artistMBID).Scan(&releaseID); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	return userID, releaseID, artistMBID
}

// NFR-2: subscribing from the bot and from the extension must collapse to one
// row. This is the requirement that the user/artist pair is unique.
func TestSubscriptionPairIsUnique(t *testing.T) {
	ctx, tx := testPool(t)
	userID, _, artistMBID := fixtures(ctx, t, tx)

	for _, source := range []string{"bot", "extension"} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO subscriptions (user_id, artist_mbid, source)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			userID, artistMBID, source); err != nil {
			t.Fatalf("insert subscription from %s: %v", source, err)
		}
	}

	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("subscriptions for one user/artist pair = %d, want 1", n)
	}
}

// NFR-1: the poller running twice over an overlapping window must not produce a
// second notification. This is what makes a generous poll window safe (D15).
func TestNotificationIsSentOnlyOnce(t *testing.T) {
	ctx, tx := testPool(t)
	userID, releaseID, _ := fixtures(ctx, t, tx)

	for i := range 3 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO notifications (user_id, release_id)
			 VALUES ($1, $2) ON CONFLICT (user_id, release_id) DO NOTHING`,
			userID, releaseID); err != nil {
			t.Fatalf("insert notification (pass %d): %v", i, err)
		}
	}

	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND release_id = $2`,
		userID, releaseID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("notifications for one user/release = %d, want 1", n)
	}
}

// D7: MusicBrainz release groups already merge every edition of an album, so
// the same release re-appearing under a different title must be a no-op.
func TestReleaseGroupDedupesEditions(t *testing.T) {
	ctx, tx := testPool(t)
	_, _, artistMBID := fixtures(ctx, t, tx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO releases
			(release_group_mbid, artist_mbid, title, primary_type, release_date, dedup_key)
		 VALUES ('b1b2c3d4-0000-0000-0000-0000000000ff', $1, 'Test Album (Deluxe)', 'Album',
		         '2026-08-20', 'a-different-dedup-key')
		 ON CONFLICT (release_group_mbid) DO NOTHING`, artistMBID); err != nil {
		t.Fatalf("insert duplicate release group: %v", err)
	}

	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM releases WHERE artist_mbid = $1`, artistMBID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("releases for one release group = %d, want 1", n)
	}
}

// FR-2.3: only Album, Single and EP are notifiable. The live ListenBrainz feed
// also carries Broadcast, Other and null types, so the database must refuse them.
func TestUnwantedReleaseTypesAreRejected(t *testing.T) {
	ctx, tx := testPool(t)
	_, _, artistMBID := fixtures(ctx, t, tx)

	for _, badType := range []string{"Broadcast", "Other", "Compilation"} {
		t.Run(badType, func(t *testing.T) {
			// Savepoint: a failed statement poisons the surrounding transaction.
			if _, err := tx.Exec(ctx, "SAVEPOINT bad_type"); err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO releases
					(release_group_mbid, artist_mbid, title, primary_type, release_date, dedup_key)
				 VALUES (gen_random_uuid(), $1, 'Nope', $2, '2026-08-20', $2)`,
				artistMBID, badType)
			if err == nil {
				t.Fatalf("primary_type %q was accepted, want rejection", badType)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT bad_type"); err != nil {
				t.Fatalf("rollback to savepoint: %v", err)
			}
		})
	}
}

// C1: a bot can only address a numeric chat id, so that column is the identity
// of a user and must not duplicate.
func TestTelegramChatIDIsUnique(t *testing.T) {
	ctx, tx := testPool(t)
	fixtures(ctx, t, tx)

	if _, err := tx.Exec(ctx, "SAVEPOINT dup_chat"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES (987654321)`); err == nil {
		t.Fatal("duplicate telegram_chat_id was accepted, want rejection")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT dup_chat"); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
}
