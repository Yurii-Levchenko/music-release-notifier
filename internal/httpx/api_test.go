package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

type fakeLinker struct {
	token     storage.LinkToken
	createIn  string
	createErr error

	// linked maps an api token to the install it resolves to. Anything absent
	// is an unredeemed or invented token.
	linked    map[string]storage.Install
	lookupErr error
	touched   []string
}

func (f *fakeLinker) CreateLinkToken(_ context.Context, installID string) (storage.LinkToken, error) {
	f.createIn = installID
	return f.token, f.createErr
}

func (f *fakeLinker) InstallByAPIToken(_ context.Context, apiToken string) (storage.Install, bool, error) {
	if f.lookupErr != nil {
		return storage.Install{}, false, f.lookupErr
	}
	in, ok := f.linked[apiToken]
	return in, ok, nil
}

func (f *fakeLinker) TouchInstall(_ context.Context, installID string) error {
	f.touched = append(f.touched, installID)
	return nil
}

type fakeCounter struct {
	n   int
	err error
}

func (f fakeCounter) CountForUser(context.Context, int64) (int, error) { return f.n, f.err }

func testAPI(t *testing.T, linker *fakeLinker, counter fakeCounter) http.Handler {
	t.Helper()
	api := NewAPI(linker, counter, func() string { return "release_radar_bot" },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.routes(mux)
	return mux
}

func decodeError(t *testing.T, body io.Reader) apiError {
	t.Helper()
	var out apiError
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out
}

func TestLinkInitReturnsADeepLinkAndACode(t *testing.T) {
	linker := &fakeLinker{token: storage.LinkToken{
		Token:     "tok-abc",
		ShortCode: "K7F2QX",
		APIToken:  "api-xyz",
		ExpiresAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	}}
	h := testAPI(t, linker, fakeCounter{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/link/init",
		strings.NewReader(`{"install_id":"abcdefgh12345678"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got linkInitResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeepLink != "https://t.me/release_radar_bot?start=tok-abc" {
		t.Errorf("deep_link = %q", got.DeepLink)
	}
	if got.ShortCode != "K7F2QX" || got.APIToken != "api-xyz" {
		t.Errorf("short_code = %q, api_token = %q", got.ShortCode, got.APIToken)
	}
	if linker.createIn != "abcdefgh12345678" {
		t.Errorf("install_id passed through as %q", linker.createIn)
	}
}

func TestLinkInitRejectsUnusableInstallIDs(t *testing.T) {
	cases := map[string]string{
		"too short":        `{"install_id":"abc"}`,
		"empty":            `{"install_id":""}`,
		"missing":          `{}`,
		"not json":         `hello`,
		"spaces":           `{"install_id":"has spaces here"}`,
		"path traversal":   `{"install_id":"../../etc/passwd"}`,
		"newline injected": "{\"install_id\":\"abcdefgh\\n12345678\"}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			linker := &fakeLinker{}
			h := testAPI(t, linker, fakeCounter{})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/link/init", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			if linker.createIn != "" {
				t.Errorf("a token was minted for %q", linker.createIn)
			}
		})
	}
}

// The credential is handed out before anybody links, so this is the property
// that makes that safe: holding it proves nothing until somebody redeems the
// matching code in Telegram.
func TestMeRejectsATokenNobodyRedeemed(t *testing.T) {
	h := testAPI(t, &fakeLinker{linked: map[string]storage.Install{}}, fakeCounter{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer api-xyz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec.Body).Error.Code; code != "not_linked" {
		t.Errorf("error code = %q, want not_linked", code)
	}
}

// A missing header, a malformed one and an invented token must be
// indistinguishable. Anything else is an oracle for probing which tokens exist.
func TestEveryAuthFailureLooksTheSame(t *testing.T) {
	h := testAPI(t, &fakeLinker{linked: map[string]storage.Install{"good": {}}}, fakeCounter{})

	var bodies []string
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic good", "Bearer nope"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", http.NoBody)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("header %q gave status %d, want 401", header, rec.Code)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("responses differ:\n  %s\n  %s", bodies[0], bodies[i])
		}
	}
}

func TestMeAnswersForALinkedInstall(t *testing.T) {
	linker := &fakeLinker{linked: map[string]storage.Install{
		"api-xyz": {InstallID: "install-1", UserID: 42, TelegramUsername: "yurii"},
	}}
	h := testAPI(t, linker, fakeCounter{n: 12})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer api-xyz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got meResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Linked || got.SubscriptionCount != 12 {
		t.Errorf("got %+v", got)
	}
	// The extension renders this straight into a menu, so the @ belongs to the
	// API rather than to every caller that forgets it.
	if got.TelegramUsername != "@yurii" {
		t.Errorf("telegram_username = %q, want @yurii", got.TelegramUsername)
	}
	if len(linker.touched) != 1 || linker.touched[0] != "install-1" {
		t.Errorf("touched = %v", linker.touched)
	}
}

// last_seen_at is a statistic. An authenticated request must not fail because
// writing it did.
func TestMeSurvivesAFailedTouch(t *testing.T) {
	linker := &failingTouch{fakeLinker{linked: map[string]storage.Install{
		"api-xyz": {InstallID: "install-1", UserID: 42},
	}}}
	api := NewAPI(linker, fakeCounter{n: 3}, func() string { return "bot" },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.routes(mux)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer api-xyz")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 despite the touch failing", rec.Code)
	}
}

type failingTouch struct{ fakeLinker }

func (f *failingTouch) TouchInstall(context.Context, string) error {
	return errors.New("database is on fire")
}

func TestPreflightIsAnsweredForTheExtensionOrigin(t *testing.T) {
	h := testAPI(t, &fakeLinker{}, fakeCounter{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/me", http.NoBody)
	req.Header.Set("Origin", extensionOrigin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != extensionOrigin {
		t.Errorf("allow-origin = %q", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Error("Authorization is not allowed, so the extension could never send its token")
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Error("Vary: Origin missing; a cache could serve one origin's answer to another")
	}
}

// A wildcard would let any page the user has open drive this API. Only the
// Spotify client's own origin gets the header.
func TestOtherOriginsGetNoCORSHeader(t *testing.T) {
	h := testAPI(t, &fakeLinker{}, fakeCounter{})

	for _, origin := range []string{
		"https://evil.example",
		"http://xpui.app.spotify.com",       // wrong scheme
		"https://xpui.app.spotify.com.evil", // suffix trick
		"https://spotify.com",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/link/init",
			strings.NewReader(`{"install_id":"abcdefgh12345678"}`))
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q was allowed as %q", origin, got)
		}
	}
}

func TestLinkInitWithNoBotDoesNotHandOutABrokenLink(t *testing.T) {
	api := NewAPI(&fakeLinker{token: storage.LinkToken{Token: "t", ShortCode: "K7F2QX"}},
		fakeCounter{}, func() string { return "" },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	api.routes(mux)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/link/init",
		strings.NewReader(`{"install_id":"abcdefgh12345678"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 rather than a t.me link to no bot", rec.Code)
	}
}
