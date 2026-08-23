// Command releaseradar is the whole application: one binary that will host the
// bot, poller, notifier and api as goroutines (SPEC.md §5). S0 wires up config,
// database, migrations, HTTP and shutdown — the workers arrive in S1 and later.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/config"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/httpx"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

func main() {
	// run() so every exit path can defer cleanly; main only sets the exit code.
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Logger is not configured yet, so use the default one.
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting release radar", "config", cfg.Redacted())

	// Cancelled on SIGINT/SIGTERM so in-flight work can wind down.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("database connected")

	if err := storage.Migrate(ctx, pool, log); err != nil {
		return err
	}

	// D13: the channel registry exists from the first commit even though no
	// channel is implemented yet. Empty here, Telegram joins in S5.
	channels := notify.NewRegistry()
	log.Info("notification channels registered", "kinds", channels.Kinds())

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.New(pool, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Serve in the background so the main goroutine can wait on ctx.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Fresh context: ctx is already cancelled, and shutdown needs to outlive it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	log.Info("stopped cleanly")
	return nil
}
