// Command releaseradar is the whole application: one binary hosting the bot,
// poller, notifier and api as goroutines (SPEC.md §5). S0 wired up config,
// database, migrations, HTTP and shutdown; S1 adds the Telegram bot worker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/config"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/health"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/httpx"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/listenbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/metrics"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/musicbrainz"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notifier"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/poller"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
	"github.com/Yurii-Levchenko/music-release-notifier/internal/telegram"
)

// searchCacheTTL is how long an artist search stays usable. MusicBrainz allows
// one request per second per IP (SPEC.md C25), and an artist's identity does not
// change week to week, so a generous window costs nothing and saves a lot.
const searchCacheTTL = 7 * 24 * time.Hour

// pollInterval is how often the release feed is read. Daily is enough: the feed
// is a window, not a stream, and the poller looks back seven days (SPEC.md D15),
// so a missed run costs nothing as long as the next one lands inside the window.
const pollInterval = 24 * time.Hour

// A claimed notification is hidden for this long. If the process dies mid-send,
// the row becomes due again after it rather than needing a cleanup job.
const claimLease = 5 * time.Minute

// After this many attempts a notification is failed rather than retried
// forever: a message nobody can receive should stop consuming rate-limit budget
// that live ones need.
const maxDeliveryAttempts = 5

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

	// Canceled on SIGINT/SIGTERM so in-flight work can wind down.
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

	// A private registry rather than prometheus.DefaultRegisterer. The default
	// one is global state that any imported library can write to, and the Go
	// runtime collectors are added explicitly below so the set is exactly what
	// this binary chose to publish.
	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector())
	promReg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	appMetrics := metrics.New(promReg)

	outbox := storage.NewNotifications(pool, claimLease, maxDeliveryAttempts)
	if err := appMetrics.RegisterQueue(promReg, outbox); err != nil {
		return fmt.Errorf("register queue metrics: %w", err)
	}
	if err := appMetrics.RegisterPollState(promReg, storage.NewReleases(pool)); err != nil {
		return fmt.Errorf("register poll state metrics: %w", err)
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
		// Refuses to build without a User-Agent carrying a contact address:
		// MusicBrainz answers an anonymous client with 403 and a generic one
		// with 503, and neither improves on retry.
		mb, err := musicbrainz.New(cfg.UserAgent, log)
		if err != nil {
			return err
		}
		mb.WithMetrics(appMetrics)
		channels = notify.NewRegistry(telegram.NewNotifier(client))
		bot = telegram.NewBot(
			client,
			storage.NewUsers(pool),
			mb,
			storage.NewSearchCache(pool, searchCacheTTL),
			storage.NewSubscriptions(pool),
			log,
		)
	}
	log.Info("notification channels registered", "kinds", channels.Kinds())

	// The poller runs whether or not Telegram is configured: detection is
	// independent of delivery, and a queued notification waits in the outbox
	// until a channel exists to carry it.
	lb, err := listenbrainz.New(cfg.UserAgent, log)
	if err != nil {
		return err
	}
	releasePoller := poller.New(lb, storage.NewReleases(pool), pollInterval, log)

	// The health registry defines what "working" means for the dead-man's
	// switch. Budgets are per worker because their rhythms differ by four
	// orders of magnitude: the bot proves itself every minute, the poller once
	// a day. A single global budget would either cry wolf over the idle poller
	// or never notice a wedged bot.
	//
	// Each budget is a small multiple of the worker's own cycle, so one missed
	// cycle is tolerated and a genuinely stuck worker is not.
	healthReg := health.NewRegistry()
	healthReg.AddProbe("database", pool.Ping)

	if bot != nil {
		botHealth := healthReg.Register("bot", 5*time.Minute)
		bot.ReportProgressTo(botHealth.Beat)
		bot.WithMetrics(appMetrics)
	}
	pollerHealth := healthReg.Register("poller", 26*time.Hour)
	releasePoller.ReportProgressTo(pollerHealth.Beat)
	releasePoller.WithMetrics(appMetrics)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.New(pool, healthReg, promReg, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// One errgroup per long-running worker. The group's context is canceled as
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

	g.Go(func() error { return releasePoller.Run(gctx) })

	// The notifier drains whatever the poller queued. It starts even with no
	// channels configured, so that "the queue is filling and nobody is draining
	// it" is a log line rather than a discovery weeks later.
	deliveries := notifier.New(outbox, channels, log).WithMetrics(appMetrics)
	notifierHealth := healthReg.Register("notifier", 5*time.Minute)
	deliveries.ReportProgressTo(notifierHealth.Beat)
	g.Go(func() error { return deliveries.Run(gctx) })

	// The dead-man's switch. It pings only while every worker above is inside
	// its budget, so silence — from a crash, a wedge, or an unreachable
	// database — is what raises the alarm. This is the one piece that could
	// have caught the 14-hour outage on 29.08.2026, because it lives outside
	// the process it watches.
	beat := health.NewHeartbeat(cfg.HeartbeatURL, cfg.HeartbeatInterval, healthReg, log)
	g.Go(func() error { return beat.Run(gctx) })

	// Shut the HTTP server down when anything else asks us to stop; without
	// this, ListenAndServe would keep the group waiting forever.
	g.Go(func() error {
		<-gctx.Done()
		// Deliberately NOT derived from gctx: gctx is already canceled at this
		// point, so a child of it would abort the shutdown instantly and drop
		// in-flight requests. contextcheck cannot see that, hence the nolint.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		//nolint:contextcheck // fresh context is required; see comment above
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
