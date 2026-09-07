package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

func newTestBrowser(t *testing.T) (*Browser, *config.Config) {
	t.Helper()
	cfg := &config.Config{
		Location: time.UTC,
		Capture:  config.Capture{SpoolDir: t.TempDir()},
		Sync:     config.Sync{Mode: config.SyncOpportunistic, ArchiveDir: t.TempDir(), Sentinel: ".nas"},
	}
	return New(cfg), cfg
}

func writeFrame(t *testing.T, root, day, name string) {
	t.Helper()
	dir := filepath.Join(root, day[:4], day[:7], day)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("jpeg"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Every component that reaches the filesystem comes from a URL, so traversal
// attempts must be rejected before any path is built.
func TestPathTraversalIsRejected(t *testing.T) {
	b, _ := newTestBrowser(t)

	badDays := []string{
		"../../../etc",
		"2026-09-07/../../..",
		"....//....//etc",
		"%2e%2e%2f",
		"2026-9-7",
		"",
	}
	for _, day := range badDays {
		if _, err := b.Frames(day); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Frames(%q) should be rejected, got %v", day, err)
		}
	}

	badNames := []string{
		"../../../etc/passwd",
		"..%2fsecret.jpg",
		"secret.txt",
		"../evil.jpg",
		"/etc/passwd",
	}
	for _, name := range badNames {
		if _, err := b.FramePath("2026-09-07", name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("FramePath(%q) should be rejected, got %v", name, err)
		}
	}

	for _, year := range []string{"../..", "20x6", "2026/.."} {
		if _, err := b.Months(year); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Months(%q) should be rejected, got %v", year, err)
		}
	}
}

// The browser must merge both roots so that frames not yet synced are still
// visible in the UI.
func TestFramesMergeArchiveAndSpool(t *testing.T) {
	b, cfg := newTestBrowser(t)
	writeFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-05-00-00.jpg")
	writeFrame(t, cfg.Capture.SpoolDir, "2026-09-07", "snapshot_2026-09-07-05-05-00.jpg")

	frames, err := b.Frames("2026-09-07")
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames across both roots, got %d", len(frames))
	}
	if frames[0].Time != "05:00:00" || frames[1].Time != "05:05:00" {
		t.Fatalf("frames are not in chronological order: %+v", frames)
	}
	if frames[0].Group != "snapshot_" {
		t.Fatalf("unexpected group %q", frames[0].Group)
	}
}

// The archive holds two decades of images from different capture methods; the
// UI needs to keep those series apart so playback does not jump between them.
func TestGroupSeparatesCaptureSeries(t *testing.T) {
	b, cfg := newTestBrowser(t)
	writeFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "steiner_attic_2026-09-07-05-00-00.jpg")
	writeFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "steiner_attic_ui_2026-09-07-05-00-00.jpg")

	frames, err := b.Frames("2026-09-07")
	if err != nil {
		t.Fatalf("Frames: %v", err)
	}

	groups := map[string]bool{}
	for _, frame := range frames {
		groups[frame.Group] = true
	}
	if !groups["steiner_attic_"] || !groups["steiner_attic_ui_"] {
		t.Fatalf("expected both series to be recognised, got %v", groups)
	}
}

func TestYearsAndMonthsAreNewestFirst(t *testing.T) {
	b, cfg := newTestBrowser(t)
	writeFrame(t, cfg.Sync.ArchiveDir, "2024-01-05", "snapshot_2024-01-05-05-00-00.jpg")
	writeFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-05-00-00.jpg")

	years := b.Years()
	if len(years) != 2 || years[0] != "2026" {
		t.Fatalf("expected the newest year first, got %v", years)
	}
}

func TestFramePathResolvesAndReportsMissing(t *testing.T) {
	b, cfg := newTestBrowser(t)
	writeFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-05-00-00.jpg")

	path, err := b.FramePath("2026-09-07", "snapshot_2026-09-07-05-00-00.jpg")
	if err != nil {
		t.Fatalf("FramePath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resolved path does not exist: %v", err)
	}

	if _, err := b.FramePath("2026-09-07", "snapshot_2026-09-07-23-59-59.jpg"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
