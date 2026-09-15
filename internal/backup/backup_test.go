package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write creates a file with a known modification time.
func write(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
	return path
}

// The atomicity guarantee in one test. The backup script dumps to <name>.tmp
// and renames only after the archive has been verified, so a half-written dump
// must not register as a backup — otherwise a job that dies mid-dump on every
// run keeps the freshness alert quiet forever.
func TestPartialDumpsDoNotCount(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "releaseradar-20260101T000000Z.dump", 100, 48*time.Hour)
	write(t, dir, "releaseradar-20260103T000000Z.dump.tmp", 999, 1*time.Minute)

	st, err := NewDir(dir).State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Count != 1 {
		t.Fatalf("counted %d dumps, want 1 — the .tmp file was counted", st.Count)
	}
	if st.SizeBytes != 100 {
		t.Fatalf("size %d, want 100 — the newest file picked was the partial one", st.SizeBytes)
	}
	if age := time.Since(st.Newest); age < 24*time.Hour {
		t.Fatalf("newest dump reported as %v old; the 1-minute .tmp file won", age)
	}
}

func TestNewestDumpWins(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.dump", 10, 72*time.Hour)
	write(t, dir, "b.dump", 20, 1*time.Hour)
	write(t, dir, "c.dump", 30, 24*time.Hour)

	st, err := NewDir(dir).State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Count != 3 {
		t.Fatalf("count = %d, want 3", st.Count)
	}
	// Size must come from the same file the timestamp came from, not from the
	// largest or the last one listed.
	if st.SizeBytes != 20 {
		t.Fatalf("size = %d, want 20 (the newest dump)", st.SizeBytes)
	}
	if got := time.Since(st.Newest).Round(time.Minute); got != time.Hour {
		t.Fatalf("newest is %v old, want ~1h", got)
	}
}

func TestOtherFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", 10, time.Hour)
	write(t, dir, "dump", 10, time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "nested.dump"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	st, err := NewDir(dir).State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Count != 0 {
		t.Fatalf("count = %d, want 0", st.Count)
	}
	if !st.Newest.IsZero() {
		t.Fatalf("newest = %v, want zero", st.Newest)
	}
}

// An empty directory and a missing one are different answers. Empty means the
// job has not produced anything yet or rotation has eaten everything — a real,
// readable fact. Missing means we could not look, and the caller must be able
// to tell them apart to avoid publishing "zero backups" when it does not know.
func TestEmptyAndMissingDiffer(t *testing.T) {
	st, err := NewDir(t.TempDir()).State()
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if st.Count != 0 || !st.Newest.IsZero() {
		t.Fatalf("empty dir gave %+v", st)
	}

	if _, err := NewDir(filepath.Join(t.TempDir(), "nope")).State(); err == nil {
		t.Fatal("missing directory returned no error, so it is indistinguishable from an empty one")
	}
}
