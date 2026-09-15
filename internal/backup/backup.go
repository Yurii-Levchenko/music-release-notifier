// Package backup reports on the database dumps another container produces.
//
// It deliberately does not make them, and it deliberately measures the files
// rather than trusting a record of the work. A job that writes "backup
// succeeded at T" into a table goes on saying so after somebody deletes the
// directory, prunes the volume or fills the disk — the claim survives the
// thing it was making a claim about. A directory listing cannot lie that way:
// if the dumps are gone, the metric goes with them, which is the behavior you
// want from the number that decides whether you still have a backup.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Extension of a finished dump. The backup script writes to <name>.dump.tmp
// and renames on success, so anything matching this suffix — and not the tmp
// one — is a dump that was written completely and then verified.
const ext = ".dump"

// State is what one look at the backup directory says.
type State struct {
	// Newest is the modification time of the most recent dump. Zero when there
	// are none.
	Newest time.Time
	// SizeBytes is the size of that newest dump. A dump that suddenly shrinks
	// by an order of magnitude is a restore problem discovered early rather
	// than during a restore.
	SizeBytes int64
	// Count is how many dumps are being retained, which is what says whether
	// rotation is working. Silent over-retention fills a disk; silent
	// over-deletion leaves one copy.
	Count int
}

// Dir reads a directory of dumps.
type Dir struct{ path string }

func NewDir(path string) *Dir { return &Dir{path: path} }

// State lists the directory and reports on the newest dump.
//
// An error means the directory could not be read at all. The caller publishes
// nothing in that case rather than a zero: there is no ambiguous zero here the
// way there is for a queue depth — a missing timestamp is caught by absent()
// in the alert, whereas a zero timestamp is 1970 and would read as a backup
// that is 56 years old.
func (d *Dir) State() (State, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return State{}, fmt.Errorf("read backup dir %s: %w", d.path, err)
	}

	var st State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Raced with rotation deleting it. Skipping is right: the file is
			// genuinely no longer there, and failing the whole scrape over one
			// pruned dump would turn routine rotation into an outage.
			continue
		}
		st.Count++
		if info.ModTime().After(st.Newest) {
			st.Newest = info.ModTime()
			st.SizeBytes = info.Size()
		}
	}
	return st, nil
}

// Path is where this reads from, for logging.
func (d *Dir) Path() string { return filepath.Clean(d.path) }
