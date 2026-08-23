// Spike S7 — throwaway server. Answers exactly one question:
// can a Spicetify extension reach our own backend with a plain fetch()?
//
// Run:  go run ./spike/server
// Then trigger the extension from Spotify and watch this log.
//
// DELETE THIS DIRECTORY once S8 builds the real API.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// The page origin inside the Spotify desktop client. Verified in the live
// client, see SPEC.md C16.
const spotifyOrigin = "https://xpui.app.spotify.com"

const addr = "127.0.0.1:8099"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", withCORS(log, healthz))
	mux.HandleFunc("/v1/spike", withCORS(log, spike))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Println("SPIKE S7 — waiting for a request from the Spicetify extension")
	fmt.Println("-------------------------------------------------------------")
	fmt.Printf("  listening   http://%s\n", addr)
	fmt.Println("  expecting   OPTIONS preflight, then POST /v1/spike")
	fmt.Printf("  allowed     %s\n", spotifyOrigin)
	fmt.Println("-------------------------------------------------------------")
	fmt.Println("Right-click an artist in Spotify. Ctrl+C to stop.")
	fmt.Println()

	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// withCORS answers the preflight and stamps the response so the browser lets
// the extension read it. Getting this wrong is the most likely reason the
// spike "fails" for a reason that is not the one we are testing.
func withCORS(log *slog.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"origin", origin,
			"ua", truncate(r.Header.Get("User-Agent"), 60),
		)

		// Echo the origin only when we recognise it. Reflecting anything back
		// would be sloppy even in throwaway code.
		if origin == spotifyOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else if origin != "" {
			// Still allow it for the spike, but say so loudly — an unexpected
			// origin is itself a finding worth seeing in the log.
			log.Warn("unexpected origin — allowing it anyway for the spike", "origin", origin)
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "600")

		if r.Method == http.MethodOptions {
			log.Info("→ preflight answered 204")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type spikeRequest struct {
	Action    string `json:"action"`
	ArtistID  string `json:"spotify_artist_id"`
	Name      string `json:"name"`
	InstallID string `json:"install_id"`
}

func spike(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	var req spikeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Printf("\n  !! body was not JSON: %s\n\n", truncate(string(raw), 200))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	// Distinguish the real thing from my own curl smoke test. A tool that
	// claims success for either is worse than no tool.
	ua := r.Header.Get("User-Agent")
	fromClient := r.Header.Get("Origin") == spotifyOrigin &&
		!strings.HasPrefix(strings.ToLower(ua), "curl/")

	fmt.Println()
	if fromClient {
		fmt.Println("  ==> SPIKE PASSED: fetch() from the Spotify client reached this server.")
		fmt.Println("      SPEC.md C15/C16 confirmed on this machine. Variant A is viable.")
	} else {
		fmt.Println("  --- local smoke test (not the Spotify client) ---")
		fmt.Println("      Server and CORS are correct, but this proves nothing about the client.")
	}
	fmt.Printf("      action     %s\n", req.Action)
	fmt.Printf("      artist_id  %s\n", req.ArtistID)
	fmt.Printf("      name       %s\n", req.Name)
	fmt.Printf("      user-agent %s\n", truncate(ua, 70))
	fmt.Println()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"action":    req.Action,
		"artist_id": req.ArtistID,
		"echo":      "hello from Go",
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
