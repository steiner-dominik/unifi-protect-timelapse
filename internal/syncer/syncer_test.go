package syncer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

func newTestSyncer(t *testing.T, sentinel bool) (*Syncer, *config.Config) {
	t.Helper()

	spool := t.TempDir()
	archive := t.TempDir()

	if sentinel {
		if err := os.WriteFile(filepath.Join(archive, ".nas"), []byte("marker"), 0o600); err != nil {
			t.Fatalf("writing sentinel: %v", err)
		}
	}

	cfg := &config.Config{
		Location: time.UTC,
		Capture:  config.Capture{SpoolDir: spool, Interval: 5 * time.Minute},
		Sync: config.Sync{
			Mode:           config.SyncOpportunistic,
			ArchiveDir:     archive,
			Sentinel:       ".nas",
			PruneEmptyDirs: true,
		},
	}

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}
	return New(cfg, store, discardLogger()), cfg
}

func writeSpoolFile(t *testing.T, cfg *config.Config, rel string, content string) string {
	t.Helper()
	path := filepath.Join(cfg.Capture.SpoolDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// The sentinel check is the guard that stops the service from writing into an
// unmounted mountpoint and deleting the originals. Without it, an NFS share
// that is merely absent looks like an empty local directory.
func TestRunRefusesWhenSentinelMissing(t *testing.T) {
	s, cfg := newTestSyncer(t, false)
	source := writeSpoolFile(t, cfg, "2026/2026-09/2026-09-07/snapshot_2026-09-07-05-00-00.jpg", "image")

	if _, err := s.Run(context.Background()); err == nil {
		t.Fatal("expected an error when the sentinel is missing")
	}

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source file must be left untouched, got %v", err)
	}
	entries, err := os.ReadDir(cfg.Sync.ArchiveDir)
	if err != nil {
		t.Fatalf("reading archive: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("nothing may be written to an unavailable archive, found %d entries", len(entries))
	}
}

func TestRunMovesFilesAndPreservesLayout(t *testing.T) {
	s, cfg := newTestSyncer(t, true)
	rel := "2026/2026-09/2026-09-06/snapshot_2026-09-06-05-00-00.jpg"
	source := writeSpoolFile(t, cfg, rel, "image-content")

	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if report.Files != 1 {
		t.Fatalf("expected 1 file moved, got %d", report.Files)
	}

	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("source file should be gone after a successful move")
	}
	moved, err := os.ReadFile(filepath.Join(cfg.Sync.ArchiveDir, rel))
	if err != nil {
		t.Fatalf("reading moved file: %v", err)
	}
	if string(moved) != "image-content" {
		t.Fatalf("content changed during the move: %q", moved)
	}
}

// A partially written frame must never be moved; capture writes to a temporary
// name and renames, so anything still carrying the temp prefix is in flight.
func TestRunSkipsTemporaryAndForeignFiles(t *testing.T) {
	s, cfg := newTestSyncer(t, true)
	tmp := writeSpoolFile(t, cfg, "2026/2026-09/2026-09-06/.tmp-123", "partial")
	other := writeSpoolFile(t, cfg, "2026/2026-09/2026-09-06/notes.txt", "text")

	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if report.Files != 0 {
		t.Fatalf("expected nothing to be moved, got %d", report.Files)
	}
	for _, path := range []string{tmp, other} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s should still exist: %v", path, err)
		}
	}
}

// Pruning must never remove the directory the current day is being written to.
func TestPruneKeepsTodaysDirectory(t *testing.T) {
	s, cfg := newTestSyncer(t, true)

	today := time.Now().In(cfg.Location)
	todayDir := filepath.Join(cfg.Capture.SpoolDir,
		today.Format("2006"), today.Format("2006-01"), today.Format("2006-01-02"))
	if err := os.MkdirAll(todayDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldDir := filepath.Join(cfg.Capture.SpoolDir, "2019", "2019-01", "2019-01-01")
	if err := os.MkdirAll(oldDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if _, err := os.Stat(todayDir); err != nil {
		t.Fatalf("today's directory must survive pruning: %v", err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("an empty directory from a past day should have been pruned")
	}
}

func TestSpoolStats(t *testing.T) {
	s, cfg := newTestSyncer(t, true)
	writeSpoolFile(t, cfg, "2026/2026-09/2026-09-06/a.jpg", "12345")
	writeSpoolFile(t, cfg, "2026/2026-09/2026-09-06/b.jpg", "678")
	writeSpoolFile(t, cfg, "2026/2026-09/2026-09-06/ignore.txt", "not an image")

	stats := s.Spool()
	if stats.Files != 2 {
		t.Fatalf("expected 2 images, got %d", stats.Files)
	}
	if stats.Bytes != 8 {
		t.Fatalf("expected 8 bytes, got %d", stats.Bytes)
	}
}
