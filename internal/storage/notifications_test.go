package storage_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// These run inside a transaction that is rolled back (testTx), which is not
// merely tidy here: Claim takes whatever is due across the whole table, so a
// test against the pool would lease rows from the live queue and bump their
// attempt counters. Rolling back undoes any lease the test happens to take.
//
// For the same reason the assertions are scoped — "my row is in this batch",
// never "this batch is exactly my row". Fixtures are backdated so they sort
// first under ORDER BY next_attempt_at and are always inside the claim limit.

const outboxLimit = 200

type outboxFixture struct {
	userID    int64
	chatID    int64
	releaseID int64
	notifID   int64
	artist    string
	group     string
}

func outboxFixtures(ctx context.Context, t *testing.T, tx pgx.Tx, chatID int64, mbid, group string) outboxFixture {
	t.Helper()

	f := outboxFixture{chatID: chatID, artist: mbid, group: group}

	if err := tx.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES ($1) RETURNING id`, chatID).
		Scan(&f.userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO artists (mbid, name) VALUES ($1, 'Radiohead')`, mbid); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO subscriptions (user_id, artist_mbid, source) VALUES ($1, $2, 'bot')`,
		f.userID, mbid); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO releases
			(release_group_mbid, artist_mbid, title, primary_type, release_date, cover_url, dedup_key)
		VALUES ($1, $2, 'A Moon Shaped Pool', 'Album', '2026-08-29',
		        'https://coverartarchive.org/release/x/front-250', $3)
		RETURNING id`, group, mbid, "outbox-test|"+group).Scan(&f.releaseID); err != nil {
		t.Fatalf("insert release: %v", err)
	}
	// Backdated far enough that this row sorts ahead of anything in the real
	// queue, so it is always within the claim limit.
	if err := tx.QueryRow(ctx, `
		INSERT INTO notifications (user_id, release_id, next_attempt_at)
		VALUES ($1, $2, now() - interval '10 years')
		RETURNING id`, f.userID, f.releaseID).Scan(&f.notifID); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	return f
}

func find(batch []storage.Pending, id int64) *storage.Pending {
	for i := range batch {
		if batch[i].ID == id {
			return &batch[i]
		}
	}
	return nil
}

func notifState(ctx context.Context, t *testing.T, tx pgx.Tx, id int64) (state string, attempts int, lastErr *string) {
	t.Helper()
	if err := tx.QueryRow(ctx,
		`SELECT state, attempts, last_error FROM notifications WHERE id = $1`, id).
		Scan(&state, &attempts, &lastErr); err != nil {
		t.Fatalf("read notification %d: %v", id, err)
	}
	return state, attempts, lastErr
}

// dueIn reports how far in the future the row is next due. Negative means due.
func dueIn(ctx context.Context, t *testing.T, tx pgx.Tx, id int64) time.Duration {
	t.Helper()
	var d time.Duration
	var seconds float64
	if err := tx.QueryRow(ctx,
		`SELECT extract(epoch FROM (next_attempt_at - now())) FROM notifications WHERE id = $1`, id).
		Scan(&seconds); err != nil {
		t.Fatalf("read next_attempt_at for %d: %v", id, err)
	}
	d = time.Duration(seconds * float64(time.Second))
	return d
}

// Claim must return everything needed to render and address the message, so
// that sending needs no further queries.
func TestClaimReturnsTheJoinedMessage(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000101,
		"aaaa1111-0000-4000-8000-000000000101", "bbbb1111-0000-4000-8000-000000000101")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := find(batch, f.notifID)
	if got == nil {
		t.Fatalf("claim did not return the due notification %d (batch of %d)", f.notifID, len(batch))
	}

	if got.ChatID != f.chatID {
		t.Errorf("ChatID = %d, want %d", got.ChatID, f.chatID)
	}
	if got.ArtistName != "Radiohead" || got.Title != "A Moon Shaped Pool" {
		t.Errorf("artist/title = %q / %q", got.ArtistName, got.Title)
	}
	if got.PrimaryType != "Album" || got.CoverURL == "" {
		t.Errorf("type = %q, cover = %q", got.PrimaryType, got.CoverURL)
	}
	// The release group, not the artist: the title in a notification is the
	// release's title, so tapping it must land on that release.
	if got.InfoURL != "https://musicbrainz.org/release-group/"+f.group {
		t.Errorf("InfoURL = %q, want the release group", got.InfoURL)
	}
	if strings.Contains(got.InfoURL, "/artist/") {
		t.Errorf("the release title still links to the artist page: %q", got.InfoURL)
	}
	// Claim counts the attempt it is handing out, so the first claim is 1.
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d on first claim, want 1", got.Attempts)
	}
}

// The lease is the whole reason there is no "sending" state. A claimed row must
// disappear from the queue for the lease duration, or two ticks of the same
// notifier would send the same message twice.
func TestClaimLeasesTheRow(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000102,
		"aaaa1111-0000-4000-8000-000000000102", "bbbb1111-0000-4000-8000-000000000102")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	first, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if find(first, f.notifID) == nil {
		t.Fatalf("first claim missed the due notification")
	}

	second, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if find(second, f.notifID) != nil {
		t.Fatal("the same notification was claimed twice inside its lease; it would be sent twice")
	}

	if d := dueIn(ctx, t, tx, f.notifID); d < 4*time.Minute {
		t.Fatalf("lease pushed next_attempt_at only %v out, want ~5m", d)
	}
}

// A lease expiring is the recovery path for a crashed worker: no cleanup job,
// the row simply becomes due again.
func TestExpiredLeaseBecomesClaimableAgain(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000103,
		"aaaa1111-0000-4000-8000-000000000103", "bbbb1111-0000-4000-8000-000000000103")

	// A lease that has already elapsed by the time we look, standing in for a
	// worker that died mid-send.
	outbox := storage.NewNotifications(tx, time.Nanosecond, 5)

	if _, err := outbox.Claim(ctx, outboxLimit); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	again, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	got := find(again, f.notifID)
	if got == nil {
		t.Fatal("a notification with an expired lease was not reclaimed; it would be stuck forever")
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d after two claims, want 2", got.Attempts)
	}
}

// Blocked users are excluded rather than claimed and skipped: their rows stay
// pending and cost nothing until they unblock the bot, at which point the
// release goes out.
func TestClaimSkipsBlockedUsers(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000104,
		"aaaa1111-0000-4000-8000-000000000104", "bbbb1111-0000-4000-8000-000000000104")

	if _, err := tx.Exec(ctx,
		`UPDATE users SET blocked_at = now() WHERE id = $1`, f.userID); err != nil {
		t.Fatalf("block user: %v", err)
	}

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)
	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if find(batch, f.notifID) != nil {
		t.Fatal("claimed a notification for a user who has blocked the bot")
	}

	// And it must still be pending, not consumed: unblocking should deliver it.
	state, attempts, _ := notifState(ctx, t, tx, f.notifID)
	if state != "pending" {
		t.Errorf("state = %q, want pending", state)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d; a skipped-over row must not burn attempts", attempts)
	}
}

func TestMarkSentClosesTheNotification(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000105,
		"aaaa1111-0000-4000-8000-000000000105", "bbbb1111-0000-4000-8000-000000000105")

	outbox := storage.NewNotifications(tx, time.Nanosecond, 5)
	if err := outbox.MarkSent(ctx, f.notifID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	state, _, _ := notifState(ctx, t, tx, f.notifID)
	if state != "sent" {
		t.Fatalf("state = %q, want sent", state)
	}

	// Even with an already-expired lease it must never come back.
	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if find(batch, f.notifID) != nil {
		t.Fatal("a sent notification was claimed again; the user would get it twice")
	}
}

// Telegram's retry_after is not advice — ignoring it escalates to longer
// lockouts, so it must win over the backoff schedule.
func TestRetryHonorsTheServersDelay(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000106,
		"aaaa1111-0000-4000-8000-000000000106", "bbbb1111-0000-4000-8000-000000000106")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	gaveUp, err := outbox.Retry(ctx, f.notifID, 1, 90*time.Second, "429 Too Many Requests")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if gaveUp {
		t.Fatal("gave up on attempt 1 of 5")
	}

	d := dueIn(ctx, t, tx, f.notifID)
	if d < 80*time.Second || d > 100*time.Second {
		t.Fatalf("next attempt in %v, want ~90s from retry_after", d)
	}

	_, _, lastErr := notifState(ctx, t, tx, f.notifID)
	if lastErr == nil || *lastErr == "" {
		t.Fatal("last_error not recorded; the reason is gone from the row")
	}
}

// Without a server-supplied delay the backoff schedule applies: 1m, 5m, 30m…
func TestRetryFallsBackToBackoff(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000107,
		"aaaa1111-0000-4000-8000-000000000107", "bbbb1111-0000-4000-8000-000000000107")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	if _, err := outbox.Retry(ctx, f.notifID, 3, 0, "connection reset"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if d := dueIn(ctx, t, tx, f.notifID); d < 25*time.Minute || d > 35*time.Minute {
		t.Fatalf("next attempt in %v, want ~30m for attempt 3", d)
	}
}

// A message nobody can receive must stop consuming rate-limit budget that live
// notifications need.
func TestRetryGivesUpAtTheAttemptCeiling(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000108,
		"aaaa1111-0000-4000-8000-000000000108", "bbbb1111-0000-4000-8000-000000000108")

	outbox := storage.NewNotifications(tx, time.Nanosecond, 5)

	gaveUp, err := outbox.Retry(ctx, f.notifID, 5, 0, "service unavailable")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !gaveUp {
		t.Fatal("did not give up at the attempt ceiling; this retries forever")
	}

	state, _, lastErr := notifState(ctx, t, tx, f.notifID)
	if state != "failed" {
		t.Errorf("state = %q, want failed", state)
	}
	if lastErr == nil || *lastErr == "" {
		t.Error("gave up without recording why")
	}

	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if find(batch, f.notifID) != nil {
		t.Fatal("a failed notification is still being claimed")
	}
}

// The destructive branch. A permanent failure must take the user's
// subscriptions and queued messages with it — and touch nobody else's.
func TestDropRecipientIsAtomicAndScoped(t *testing.T) {
	ctx, tx := testTx(t)
	victim := outboxFixtures(ctx, t, tx, -999000109,
		"aaaa1111-0000-4000-8000-000000000109", "bbbb1111-0000-4000-8000-000000000109")
	bystander := outboxFixtures(ctx, t, tx, -999000110,
		"aaaa1111-0000-4000-8000-000000000110", "bbbb1111-0000-4000-8000-000000000110")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	if err := outbox.DropRecipient(ctx, victim.userID, "Forbidden: bot was blocked by the user"); err != nil {
		t.Fatalf("drop recipient: %v", err)
	}

	var blocked *time.Time
	if err := tx.QueryRow(ctx, `SELECT blocked_at FROM users WHERE id = $1`, victim.userID).
		Scan(&blocked); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if blocked == nil {
		t.Error("user was not marked blocked")
	}

	var subs int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1`, victim.userID).Scan(&subs); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subs != 0 {
		t.Errorf("subscriptions left = %d, want 0", subs)
	}

	if state, _, _ := notifState(ctx, t, tx, victim.notifID); state != "skipped" {
		// skipped, not failed: nothing was wrong with the message.
		t.Errorf("queued notification state = %q, want skipped", state)
	}

	// Nobody else may be touched by one dead chat.
	var otherSubs int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM subscriptions WHERE user_id = $1`, bystander.userID).Scan(&otherSubs); err != nil {
		t.Fatalf("count bystander subscriptions: %v", err)
	}
	if otherSubs != 1 {
		t.Errorf("bystander subscriptions = %d, want 1 — a drop leaked across users", otherSubs)
	}
	if state, _, _ := notifState(ctx, t, tx, bystander.notifID); state != "pending" {
		t.Errorf("bystander notification state = %q, want pending", state)
	}
}

// Depth and the age of the oldest pending row are the two numbers an alert
// watches (SPEC §15a). Asserted as a delta, because the count is table-wide.
func TestQueueDepthCountsPendingWork(t *testing.T) {
	ctx, tx := testTx(t)
	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)

	before, _, err := outbox.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}

	f := outboxFixtures(ctx, t, tx, -999000111,
		"aaaa1111-0000-4000-8000-000000000111", "bbbb1111-0000-4000-8000-000000000111")

	after, oldest, err := outbox.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if after != before+1 {
		t.Fatalf("depth went %d → %d after adding one pending row", before, after)
	}
	// The fixture is backdated ten years, so it is now the oldest.
	if oldest < 24*time.Hour {
		t.Fatalf("oldest = %v, want the backdated fixture's age", oldest)
	}

	if err := outbox.MarkSent(ctx, f.notifID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	sentDepth, _, err := outbox.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("queue depth: %v", err)
	}
	if sentDepth != before {
		t.Fatalf("depth = %d after sending, want back to %d", sentDepth, before)
	}
}

// The links have to survive the whole way from the artist row to a claimed
// notification, because the notifier does no lookups of its own — the same
// property cover_url has (D14). A join that silently returns nothing would
// leave every notification without its listen row and nothing would fail.
func TestClaimCarriesTheArtistsListenLinks(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000120,
		"aaaa1111-0000-4000-8000-000000000120", "bbbb1111-0000-4000-8000-000000000120")

	if _, err := tx.Exec(ctx, `
		UPDATE artists
		SET links = $2::jsonb, links_fetched_at = now()
		WHERE mbid = $1`, f.artist,
		`{"spotify":"https://open.spotify.com/artist/S","youtube":"https://youtube.com/channel/Y"}`,
	); err != nil {
		t.Fatalf("set links: %v", err)
	}

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)
	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := find(batch, f.notifID)
	if got == nil {
		t.Fatalf("claim missed notification %d", f.notifID)
	}

	if got.Spotify != "https://open.spotify.com/artist/S" {
		t.Errorf("Spotify = %q", got.Spotify)
	}
	if got.YouTube != "https://youtube.com/channel/Y" {
		t.Errorf("YouTube = %q", got.YouTube)
	}
	// Absent in the jsonb, so it must come back empty rather than as "null".
	if got.AppleMusic != "" {
		t.Errorf("AppleMusic = %q, want empty for a key that is not there", got.AppleMusic)
	}
}

// An artist with no links must still be claimable. The join is the only path
// notifications take, so a missing key must not drop the row.
func TestClaimWorksForAnArtistWithNoLinks(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000121,
		"aaaa1111-0000-4000-8000-000000000121", "bbbb1111-0000-4000-8000-000000000121")

	outbox := storage.NewNotifications(tx, 5*time.Minute, 5)
	batch, err := outbox.Claim(ctx, outboxLimit)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got := find(batch, f.notifID)
	if got == nil {
		t.Fatal("an artist with no links lost its notification entirely")
	}
	if got.Spotify != "" || got.YouTube != "" || got.AppleMusic != "" {
		t.Errorf("links invented from an empty jsonb: %+v", got)
	}
}

// The distinction that stops an endless re-fetch: '{}' means two different
// things, and only links_fetched_at tells them apart.
func TestLinkFetchStateSeparatesNeverLookedFromNoneFound(t *testing.T) {
	ctx, tx := testTx(t)
	f := outboxFixtures(ctx, t, tx, -999000122,
		"aaaa1111-0000-4000-8000-000000000122", "bbbb1111-0000-4000-8000-000000000122")

	var fetched *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT links_fetched_at FROM artists WHERE mbid = $1`, f.artist).Scan(&fetched); err != nil {
		t.Fatalf("read: %v", err)
	}
	if fetched != nil {
		t.Fatal("a fresh artist is marked as already looked up")
	}

	// An artist that genuinely has none: empty links, but recorded as looked
	// at. Without this the lookup would repeat on every subscribe forever.
	if _, err := tx.Exec(ctx, `
		UPDATE artists SET links = '{}'::jsonb, links_fetched_at = now()
		WHERE mbid = $1`, f.artist); err != nil {
		t.Fatalf("mark fetched: %v", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT links_fetched_at FROM artists WHERE mbid = $1`, f.artist).Scan(&fetched); err != nil {
		t.Fatalf("read: %v", err)
	}
	if fetched == nil {
		t.Fatal("an artist with no links is indistinguishable from one never checked")
	}
}
