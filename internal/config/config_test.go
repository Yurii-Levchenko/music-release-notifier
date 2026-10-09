package config

import (
	"strings"
	"testing"
)

// validEnv sets everything Load requires except the heartbeat, which each
// test decides for itself.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/x")
	t.Setenv("USER_AGENT", "ReleaseRadar/0.1 ( test@example.com )")
	t.Setenv("HEARTBEAT_URL", "")
	t.Setenv("HEARTBEAT_DISABLED", "")
}

// The 16.09 incident: a misspelled key left HEARTBEAT_URL empty, and the
// process ran on unmonitored after one WARN line. It must not start at all.
func TestMissingHeartbeatURLRefusesToStart(t *testing.T) {
	validEnv(t)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "HEARTBEAT_URL is required") {
		t.Fatalf("Load() error = %v, want it to require HEARTBEAT_URL", err)
	}
}

func TestHeartbeatCanBeDisabledExplicitly(t *testing.T) {
	validEnv(t)
	t.Setenv("HEARTBEAT_DISABLED", "1")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() with HEARTBEAT_DISABLED=1: %v", err)
	}
	if !c.HeartbeatDisabled || c.HeartbeatURL != "" {
		t.Fatalf("got disabled=%v url=%q, want disabled with no URL", c.HeartbeatDisabled, c.HeartbeatURL)
	}
}

// Only "1" disables. Anybody who typed "false" or "0" meant the opposite.
func TestOnlyOneDisablesTheHeartbeat(t *testing.T) {
	for _, v := range []string{"true", "0", "false", "yes"} {
		validEnv(t)
		t.Setenv("HEARTBEAT_DISABLED", v)
		if _, err := Load(); err == nil {
			t.Errorf("HEARTBEAT_DISABLED=%q was taken as disabling the heartbeat", v)
		}
	}
}

// Both set is a contradiction; guessing which one was meant would be wrong
// half the time, and one of the two guesses is "unmonitored".
func TestURLAndDisabledTogetherIsAnError(t *testing.T) {
	validEnv(t)
	t.Setenv("HEARTBEAT_URL", "https://hc-ping.com/x")
	t.Setenv("HEARTBEAT_DISABLED", "1")

	if _, err := Load(); err == nil {
		t.Fatal("HEARTBEAT_URL with HEARTBEAT_DISABLED=1 was accepted")
	}
}

func TestHeartbeatURLIsEnough(t *testing.T) {
	validEnv(t)
	t.Setenv("HEARTBEAT_URL", "https://hc-ping.com/x")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() with a heartbeat URL: %v", err)
	}
}
