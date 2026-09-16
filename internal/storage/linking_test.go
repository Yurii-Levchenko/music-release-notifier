package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

func linkUser(ctx context.Context, t *testing.T, tx pgx.Tx, chatID int64) int64 {
	t.Helper()
	var id int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id, telegram_username) VALUES ($1, 'yurii') RETURNING id`,
		chatID).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// The invariant the whole handshake rests on: a link code binds one install to
// one account, once. Two people racing the same code — or one person
// double-tapping a deep link — must not both succeed, because the second would
// silently move somebody else's extension onto their own account.
func TestLinkTokenIsSingleUse(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)

	alice := linkUser(ctx, t, tx, -9001)
	bob := linkUser(ctx, t, tx, -9002)

	tok, err := linking.CreateLinkToken(ctx, "install-single-use")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := linking.Redeem(ctx, tok.Token, alice); err != nil {
		t.Fatalf("first redeem should succeed: %v", err)
	}
	_, err = linking.Redeem(ctx, tok.Token, bob)
	if !errors.Is(err, storage.ErrLinkNotFound) {
		t.Fatalf("second redeem returned %v, want ErrLinkNotFound", err)
	}

	// And the install must still belong to whoever got there first.
	in, found, err := linking.InstallByAPIToken(ctx, tok.APIToken)
	if err != nil || !found {
		t.Fatalf("lookup after redeem: found=%v err=%v", found, err)
	}
	if in.UserID != alice {
		t.Fatalf("install belongs to user %d, want alice %d", in.UserID, alice)
	}
}

// The same guarantee under real concurrency rather than in sequence. The single
// UPDATE ... WHERE redeemed_at IS NULL is what provides it; a
// SELECT-then-UPDATE would pass the test above and fail this one.
func TestConcurrentRedemptionHasOneWinner(t *testing.T) {
	if testDBPool == nil {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Not in a transaction: concurrent statements inside one transaction are
	// serialized by the connection, which would make the race untestable.
	// Cleaned up explicitly instead.
	linking := storage.NewLinking(testDBPool)

	var alice, bob int64
	if err := testDBPool.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES (-9101) RETURNING id`).Scan(&alice); err != nil {
		t.Fatalf("insert alice: %v", err)
	}
	if err := testDBPool.QueryRow(ctx,
		`INSERT INTO users (telegram_chat_id) VALUES (-9102) RETURNING id`).Scan(&bob); err != nil {
		t.Fatalf("insert bob: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDBPool.Exec(ctx, `DELETE FROM users WHERE telegram_chat_id IN (-9101, -9102)`)
		_, _ = testDBPool.Exec(ctx, `DELETE FROM link_tokens WHERE install_id = 'install-race'`)
		_, _ = testDBPool.Exec(ctx, `DELETE FROM installs WHERE install_id = 'install-race'`)
	})

	tok, err := linking.CreateLinkToken(ctx, "install-race")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i, user := range []int64{alice, bob} {
		wg.Add(1)
		go func(idx int, uid int64) {
			defer wg.Done()
			<-start
			_, results[idx] = linking.Redeem(ctx, tok.Token, uid)
		}(i, user)
	}
	close(start)
	wg.Wait()

	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, storage.ErrLinkNotFound):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d redemptions succeeded, want exactly 1", wins)
	}
}

func TestShortCodeRedeemsLikeTheToken(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	user := linkUser(ctx, t, tx, -9003)

	tok, err := linking.CreateLinkToken(ctx, "install-shortcode")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Typed by hand, so lower case and stray spaces are the normal case rather
	// than the exception.
	install, err := linking.Redeem(ctx, "  "+lower(tok.ShortCode)+"  ", user)
	if err != nil {
		t.Fatalf("redeem by short code: %v", err)
	}
	if install != "install-shortcode" {
		t.Fatalf("redeemed install = %q", install)
	}
}

func TestExpiredTokenCannotBeRedeemed(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	user := linkUser(ctx, t, tx, -9004)

	tok, err := linking.CreateLinkToken(ctx, "install-expired")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE link_tokens SET expires_at = now() - interval '1 second' WHERE install_id = $1`,
		"install-expired"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := linking.Redeem(ctx, tok.Token, user); !errors.Is(err, storage.ErrLinkNotFound) {
		t.Fatalf("expired token redeemed with %v, want ErrLinkNotFound", err)
	}
}

// The API token is handed out at /link/init, before anybody has linked
// anything. That is only safe if it authenticates nothing until the matching
// link token is redeemed.
func TestAPITokenIsInertUntilRedeemed(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	user := linkUser(ctx, t, tx, -9005)

	tok, err := linking.CreateLinkToken(ctx, "install-inert")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, found, err := linking.InstallByAPIToken(ctx, tok.APIToken)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found {
		t.Fatal("an unredeemed api token authenticated; it must be inert until redemption")
	}

	if _, err := linking.Redeem(ctx, tok.Token, user); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	in, found, err := linking.InstallByAPIToken(ctx, tok.APIToken)
	if err != nil || !found {
		t.Fatalf("after redeem: found=%v err=%v", found, err)
	}
	if in.UserID != user || in.TelegramChatID != -9005 || in.TelegramUsername != "yurii" {
		t.Fatalf("install = %+v", in)
	}
}

// Linking the same install again — a second machine, or somebody handing the
// extension to a friend — must move it wholesale and leave no working
// credential behind for the previous owner.
func TestRelinkingReplacesTheOldCredential(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	alice := linkUser(ctx, t, tx, -9006)
	bob := linkUser(ctx, t, tx, -9007)

	first, err := linking.CreateLinkToken(ctx, "install-relink")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := linking.Redeem(ctx, first.Token, alice); err != nil {
		t.Fatalf("redeem first: %v", err)
	}

	second, err := linking.CreateLinkToken(ctx, "install-relink")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := linking.Redeem(ctx, second.Token, bob); err != nil {
		t.Fatalf("redeem second: %v", err)
	}

	if _, found, _ := linking.InstallByAPIToken(ctx, first.APIToken); found {
		t.Fatal("the first api token still works after relinking; it is a live credential for the wrong account")
	}
	in, found, err := linking.InstallByAPIToken(ctx, second.APIToken)
	if err != nil || !found {
		t.Fatalf("second token: found=%v err=%v", found, err)
	}
	if in.UserID != bob {
		t.Fatalf("install belongs to %d, want bob %d", in.UserID, bob)
	}
}

// /stop deletes the user, and SPEC C30 requires that to take everything with
// it. An install that outlived its user would be a bearer token pointing at a
// row that no longer exists.
func TestForgettingAUserRemovesTheirInstall(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	user := linkUser(ctx, t, tx, -9008)

	tok, err := linking.CreateLinkToken(ctx, "install-cascade")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := linking.Redeem(ctx, tok.Token, user); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	if _, found, err := linking.InstallByAPIToken(ctx, tok.APIToken); err != nil || found {
		t.Fatalf("install survived the user: found=%v err=%v", found, err)
	}
}

func TestExpiredTokensAreCleanedUpButRedeemedOnesStay(t *testing.T) {
	ctx, tx := testTx(t)
	linking := storage.NewLinking(tx)
	user := linkUser(ctx, t, tx, -9009)

	kept, err := linking.CreateLinkToken(ctx, "install-kept")
	if err != nil {
		t.Fatalf("create kept: %v", err)
	}
	if _, err := linking.Redeem(ctx, kept.Token, user); err != nil {
		t.Fatalf("redeem kept: %v", err)
	}
	if _, err := linking.CreateLinkToken(ctx, "install-stale"); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	// Backdate both, so the query has to discriminate on redeemed_at and not
	// merely on age.
	if _, err := tx.Exec(ctx,
		`UPDATE link_tokens SET expires_at = now() - interval '1 hour'
		 WHERE install_id IN ('install-kept', 'install-stale')`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := linking.DeleteExpiredLinkTokens(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var keptRows, staleRows int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE install_id = 'install-kept'),
		        count(*) FILTER (WHERE install_id = 'install-stale')
		 FROM link_tokens`).Scan(&keptRows, &staleRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if staleRows != 0 {
		t.Errorf("expired unredeemed token survived cleanup")
	}
	if keptRows != 1 {
		t.Errorf("redeemed token was deleted; it is the record of when this install was linked")
	}
}

func TestLooksLikeShortCode(t *testing.T) {
	yes := []string{"K7F2QX", "k7f2qx", "  K7F2QX  ", "234567"}
	no := []string{
		"", "K7F2Q", "K7F2QXX",
		"K7F2Q0", // 0 is not in the alphabet, by design
		"K7F2QO", // nor O
		"K7F2Q1", "K7F2QI", "K7F2QL",
		"BONES!", "Camilo", // a six-letter artist name must stay searchable
	}
	for _, s := range yes {
		if !storage.LooksLikeShortCode(s) {
			t.Errorf("LooksLikeShortCode(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if storage.LooksLikeShortCode(s) {
			t.Errorf("LooksLikeShortCode(%q) = true, want false", s)
		}
	}
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + ('a' - 'A')
		}
	}
	return string(out)
}
