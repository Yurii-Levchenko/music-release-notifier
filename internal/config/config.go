// Package config reads settings from the environment. No config files: on a
// single VPS running docker compose, env vars are the whole story (NFR-8).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL      string
	HTTPAddr         string
	TelegramBotToken string
	// UserAgent is sent to MusicBrainz and ListenBrainz. MusicBrainz rejects
	// requests without a meaningful one (SPEC C25), so this is required.
	UserAgent string
	LogLevel  slog.Level

	// HeartbeatURL is an external dead-man's switch (healthchecks.io or
	// similar). Optional, and the process says so loudly when it is unset:
	// an alert raised from inside the application cannot report that the
	// application is dead (SPEC §15a).
	HeartbeatURL      string
	HeartbeatInterval time.Duration

	// BackupDir is where the backup container writes its dumps, mounted
	// read-only. Optional: unset simply means the backup metrics are not
	// published. It is a directory rather than a flag because what gets
	// measured is the files themselves — a process that recorded "a backup
	// happened" would keep saying so after the dumps were deleted.
	BackupDir string
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		HTTPAddr:         envOr("HTTP_ADDR", ":8080"),
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		UserAgent:        os.Getenv("USER_AGENT"),
		LogLevel:         parseLevel(envOr("LOG_LEVEL", "info")),
		HeartbeatURL:     os.Getenv("HEARTBEAT_URL"),
		BackupDir:        os.Getenv("BACKUP_DIR"),
	}

	interval, err := parseDuration(envOr("HEARTBEAT_INTERVAL", "5m"))
	if err != nil {
		return Config{}, fmt.Errorf("config: HEARTBEAT_INTERVAL: %w", err)
	}
	c.HeartbeatInterval = interval

	var problems []string
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if c.UserAgent == "" {
		problems = append(problems,
			"USER_AGENT is required (MusicBrainz rejects requests without a meaningful one)")
	} else if !strings.Contains(c.UserAgent, "@") {
		// MusicBrainz asks for contact details. A UA with no way to reach you
		// is the kind of thing that gets an IP blocked rather than emailed.
		problems = append(problems,
			"USER_AGENT should include a contact email, e.g. ReleaseRadar/0.1 ( you@example.com )")
	}
	// TELEGRAM_BOT_TOKEN is deliberately not required yet — S0 has no bot.

	if len(problems) > 0 {
		return Config{}, errors.New("config: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// Redacted renders the config for logging with nothing secret in it.
func (c Config) Redacted() string {
	return fmt.Sprintf(
		"http=%s db=%s telegram_token=%s user_agent=%q log_level=%s heartbeat=%s interval=%s backups=%s",
		c.HTTPAddr, redactDSN(c.DatabaseURL), present(c.TelegramBotToken),
		c.UserAgent, c.LogLevel, present(c.HeartbeatURL), c.HeartbeatInterval,
		present(c.BackupDir),
	)
}

func present(s string) string {
	if s == "" {
		return "unset"
	}
	return "set"
}

// redactDSN strips the password from a postgres URL so a DSN can be logged.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return "set"
	}
	return dsn[:scheme+3] + "***@" + dsn[at+1:]
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// parseDuration accepts Go duration syntax and refuses anything that would
// silently disable monitoring: zero and negative values are rejected rather
// than defaulted, because a heartbeat that never fires looks identical to a
// dead process and would train someone to ignore the alert.
func parseDuration(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %q", raw)
	}
	return d, nil
}
