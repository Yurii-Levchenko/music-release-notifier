// Command releaseradar is the whole application: one binary hosting the bot,
// poller, notifier and api as goroutines (SPEC.md §5). S0 wired up config,
// database, migrations, HTTP and shutdown; S1 adds the Telegram bot worker.
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

	"golang.org/x/sync/errgroup"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/config"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/httpx"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/telegram"
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

	// D13: delivery channels are plugins. The registry is what the notifier
	// worker will resolve against in S5; nothing above it knows about Telegram.
	var channels *notify.Registry
	var bot *telegram.Bot

	if cfg.TelegramBotToken == "" {
		// The app must still boot without a token: S0's compose default has
		// none, and a missing token should not take down /healthz.
		log.Warn("TELEGRAM_BOT_TOKEN is not set — bot disabled, HTTP only")
		channels = notify.NewRegistry()
	} else {
		client, err := telegram.NewClient(cfg.TelegramBotToken, log)
		if err != nil {
			return err
		}
		// Fail fast on a bad token rather than on the first send.
		if err := client.Init(ctx); err != nil {
			return err
		}
		channels = notify.NewRegistry(telegram.NewNotifier(client))
		bot = telegram.NewBot(client, storage.NewUsers(pool), log)
	}
	log.Info("notification channels registered", "kinds", channels.Kinds())

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.New(pool, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// One errgroup per long-running worker. The group's context is cancelled as
	// soon as any worker returns an error, which is what makes the whole binary
	// stop together instead of limping on half-dead.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	if bot != nil {
		g.Go(func() error { return bot.Run(gctx) })
	}

	// Shut the HTTP server down when anything else asks us to stop; without
	// this, ListenAndServe would keep the group waiting forever.
	g.Go(func() error {
		<-gctx.Done()
		// Fresh context: gctx is already cancelled and shutdown must outlive it.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("http shutdown", "err", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("stopped cleanly")
	return nil
}
