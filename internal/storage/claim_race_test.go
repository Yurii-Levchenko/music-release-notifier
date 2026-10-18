package storage_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// requireDisposableDB skips unless the database is one whose rows nobody
// delivers: CI's throwaway Postgres, or a local database named *_test. A test
// that commits due notifications into the live database would hand them to the
// running notifier, which would try to send them.
func requireDisposableDB(ctx context.Context, t *testing.T) {
	t.Helper()
	if testDBPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if os.Getenv("CI") == "true" {
		return
	}
	var name string
	if err := testDBPool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("current database: %v", err)
	}
	if !strings.HasSuffix(name, "_test") {
		t.Skipf("database %q is not a *_test database; this test commits due notifications", name)
	}
}

// Two workers claiming at the same moment must take different rows. That is
// the whole of FOR UPDATE SKIP LOCKED's job, and until now it was asserted
// only by reading the SQL: the other outbox tests run inside one transaction,
// where statements are serialized by the connection and no race can happen.
func TestConcurrentClaimsTakeDisjointRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requireDisposableDB(ctx, t)

	const (
		chat   int64 = -999000140
		artist       = "cccc2222-0000-4000-8000-000000000140"
		rows         = 40
	)
	// Its own context: t.Cleanup runs after the deferred cancel above, and a
	// cleanup on a canceled context fails silently and leaves the rows behind.
	cleanup := func() {
		bg := context.Background()
		_, _ = testDBPool.Exec(bg, `DELETE FROM users WHERE telegram_chat_id = $1`, chat)
		_, _ = testDBPool.Exec(bg, `DELETE FROM releases WHERE artist_mbid = $1`, artist)
		_, _ = testDBPool.Exec(bg, `DELETE FROM artists WHERE mbid = $1`, artist)
	}
	cleanup()
	t.Cleanup(cleanup)

	var userID int64
	if err := testDBPool.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES ($1) RETURNING id`, chat).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := testDBPool.Exec(ctx,
		`INSERT INTO artists (mbid, name) VALUES ($1, 'Race Artist')`, artist); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	ours := make(map[int64]bool, rows)
	for i := range rows {
		group := fmt.Sprintf("dddd2222-0000-4000-8000-%012d", 140000+i)
		var releaseID, notifID int64
		if err := testDBPool.QueryRow(ctx, `
			INSERT INTO releases (release_group_mbid, artist_mbid, title, primary_type, release_date, dedup_key)
			VALUES ($1, $2, $3, 'Single', '2026-10-18', $4)
			RETURNING id`, group, artist, fmt.Sprintf("Race %d", i), "race|"+group).Scan(&releaseID); err != nil {
			t.Fatalf("insert release: %v", err)
		}
		if err := testDBPool.QueryRow(ctx, `
			INSERT INTO notifications (user_id, release_id, next_attempt_at)
			VALUES ($1, $2, now() - interval '1 hour')
			RETURNING id`, userID, releaseID).Scan(&notifID); err != nil {
			t.Fatalf("insert notification: %v", err)
		}
		ours[notifID] = true
	}

	// Each worker asks for more than half, so the two must overlap unless the
	// locking keeps them apart.
	const workers = 2
	claimed := make([][]storage.Pending, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outbox := storage.NewNotifications(testDBPool, 5*time.Minute, 6)
			<-start
			claimed[w], errs[w] = outbox.Claim(ctx, rows*3/4)
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[int64]int)
	for w := range workers {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		for _, p := range claimed[w] {
			if ours[p.ID] {
				seen[p.ID]++
			}
		}
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("notification %d was claimed by %d workers; it would be sent %d times", id, n, n)
		}
	}
	if len(seen) != rows {
		t.Fatalf("claimed %d of %d due rows between two workers asking for %d each", len(seen), rows, rows*3/4)
	}
}
