package httpx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// The extension API (SPEC §7). S8 covers the handshake only: minting a link
// code and answering "am I linked". The subscription endpoints are S9.

// extensionOrigin is the single origin allowed to call this.
//
// Spicetify code runs inside the Spotify desktop client, which serves its UI
// from this origin (C16, confirmed by the S7 spike: the preflight arrived with
// exactly this Origin and a Chromium UA). A wildcard would let any web page the
// user has open call an API that manages their subscriptions.
const extensionOrigin = "https://xpui.app.spotify.com"

// Linker is the slice of storage this API needs.
type Linker interface {
	CreateLinkToken(ctx context.Context, installID string) (storage.LinkToken, error)
	InstallByAPIToken(ctx context.Context, apiToken string) (storage.Install, bool, error)
	TouchInstall(ctx context.Context, installID string) error
}

// SubscriptionCounter answers how many artists a user follows, for /me.
type SubscriptionCounter interface {
	CountForUser(ctx context.Context, userID int64) (int, error)
}

// API holds what the extension endpoints need. Separate from Server's health
// dependencies so the two cannot quietly grow into each other.
type API struct {
	linking     Linker
	subs        SubscriptionCounter
	botUsername func() string
	log         *slog.Logger
}

func NewAPI(linking Linker, subs SubscriptionCounter, botUsername func() string, log *slog.Logger) *API {
	return &API{linking: linking, subs: subs, botUsername: botUsername, log: log}
}

// apiError is the one error shape for every endpoint (SPEC §7).
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body apiError
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// routes registers the versioned API under the caller's mux.
func (a *API) routes(mux *http.ServeMux) {
	mux.Handle("POST /v1/link/init", a.cors(http.HandlerFunc(a.linkInit)))
	mux.Handle("GET /v1/me", a.cors(a.authenticated(a.me)))

	// Go's mux matches by method, so a preflight would 405 without this. The
	// browser never sees the 405 as a useful error — the request simply fails
	// with a CORS message that points nowhere near the cause.
	mux.Handle("OPTIONS /v1/", a.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
}

// cors answers preflights and tags responses for the one allowed origin.
//
// The header is only set when the request actually carries that Origin, so a
// curl with no Origin gets a plain response rather than one claiming an origin
// it never asked about.
func (a *API) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == extensionOrigin {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", extensionOrigin)
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			// Origin decides the response, so caches must not serve one
			// origin's answer to another.
			h.Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticated resolves the bearer token and puts the install in the context.
//
// Every failure answers the same 401 with the same body. An endpoint that
// distinguished "no such token" from "token not linked yet" would confirm which
// guesses are real, and the extension cannot act on the difference: in both
// cases it shows the link prompt.
func (a *API) authenticated(next func(http.ResponseWriter, *http.Request, storage.Install)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "not_linked", "Telegram is not connected yet")
			return
		}

		install, found, err := a.linking.InstallByAPIToken(r.Context(), token)
		if err != nil {
			a.log.Error("resolve api token", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on our side")
			return
		}
		if !found {
			writeError(w, http.StatusUnauthorized, "not_linked", "Telegram is not connected yet")
			return
		}

		// Best effort, and deliberately not fatal: last_seen_at is a statistic,
		// and failing an authenticated request because a statistic could not be
		// written would be the tail wagging the dog.
		if err := a.linking.TouchInstall(r.Context(), install.InstallID); err != nil {
			a.log.Warn("touch install", "err", err)
		}
		next(w, r, install)
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

type linkInitRequest struct {
	InstallID string `json:"install_id"`
}

type linkInitResponse struct {
	DeepLink  string    `json:"deep_link"`
	ShortCode string    `json:"short_code"`
	APIToken  string    `json:"api_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// maxInstallID bounds what an unauthenticated caller can store.
//
// install_id is no longer a credential — it names one extension install so a
// re-link can replace the right row — but it is still attacker-supplied text
// that lands in a database column, so it gets a length and a character set.
const maxInstallID = 128

const minInstallID = 8

func (a *API) linkInit(w http.ResponseWriter, r *http.Request) {
	// 4 KiB is far more than {"install_id":"<128 chars>"} needs, and it means a
	// body that never ends cannot hold a connection open.
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "Could not read the request body")
		return
	}

	var req linkInitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_token", "Body must be JSON")
		return
	}
	if !validInstallID(req.InstallID) {
		writeError(w, http.StatusBadRequest, "invalid_token",
			"install_id must be 8-128 characters of [A-Za-z0-9._~-]")
		return
	}

	token, err := a.linking.CreateLinkToken(r.Context(), req.InstallID)
	if err != nil {
		a.log.Error("create link token", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "Could not start linking")
		return
	}

	username := ""
	if a.botUsername != nil {
		username = a.botUsername()
	}
	if username == "" {
		// Without a bot there is no deep link to hand out, and returning one
		// that points at t.me/?start=… would be a link that silently does
		// nothing.
		a.log.Error("link init with no bot username configured")
		writeError(w, http.StatusServiceUnavailable, "internal", "The bot is not available right now")
		return
	}

	writeJSON(w, http.StatusOK, linkInitResponse{
		DeepLink:  "https://t.me/" + username + "?start=" + token.Token,
		ShortCode: token.ShortCode,
		APIToken:  token.APIToken,
		ExpiresAt: token.ExpiresAt.UTC(),
	})
}

type meResponse struct {
	Linked            bool   `json:"linked"`
	TelegramUsername  string `json:"telegram_username,omitempty"`
	SubscriptionCount int    `json:"subscription_count"`
}

// me is what the extension polls after showing the code: the api_token it was
// handed at /link/init starts returning 200 the moment somebody redeems.
func (a *API) me(w http.ResponseWriter, r *http.Request, install storage.Install) {
	count, err := a.subs.CountForUser(r.Context(), install.UserID)
	if err != nil {
		a.log.Error("count subscriptions", "err", err, "user_id", install.UserID)
		writeError(w, http.StatusInternalServerError, "internal", "Something went wrong on our side")
		return
	}

	username := install.TelegramUsername
	if username != "" && !strings.HasPrefix(username, "@") {
		username = "@" + username
	}

	writeJSON(w, http.StatusOK, meResponse{
		Linked:            true,
		TelegramUsername:  username,
		SubscriptionCount: count,
	})
}

func validInstallID(s string) bool {
	if len(s) < minInstallID || len(s) > maxInstallID {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return false
		}
	}
	return true
}
