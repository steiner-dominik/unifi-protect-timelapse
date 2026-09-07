package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

type stubSource struct {
	data  []byte
	err   error
	calls int
}

func (s *stubSource) Snapshot(context.Context) ([]byte, error) {
	s.calls++
	return s.data, s.err
}
func (s *stubSource) Describe() string { return "stub" }

func newTestMonitor(t *testing.T, source *stubSource) (*Monitor, *config.Config, *state.Store) {
	t.Helper()

	cfg := &config.Config{
		Location: time.UTC,
		// The watchdog deployment: no capturing, no syncing.
		Capture: config.Capture{Enabled: false, Interval: 5 * time.Minute, SpoolDir: t.TempDir()},
		Sync:    config.Sync{Mode: config.SyncOff, ArchiveDir: t.TempDir()},
		Camera:  config.Camera{Timeout: time.Second, Retries: 1},
		Monitor: config.Monitor{Enabled: true, Interval: time.Minute, ArchiveMaxAge: 15 * time.Minute},
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var src = source
	if src == nil {
		return New(cfg, nil, store, log), cfg, store
	}
	return New(cfg, src, store, log), cfg, store
}

func writeArchiveFrame(t *testing.T, root, day, name string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, day[:4], day[:7], day)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("jpeg"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return path
}

// The probe must never write anything: a watchdog running alongside the real
// capturer must not add frames to the archive or disturb its spacing.
func TestProbeWritesNothing(t *testing.T) {
	source := &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}}
	m, cfg, store := newTestMonitor(t, source)

	m.Probe(context.Background())

	for _, dir := range []string{cfg.Capture.SpoolDir, cfg.Sync.ArchiveDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s should be untouched, found %d entries", dir, len(entries))
		}
	}
	if data := store.Get(); !data.CameraOnline {
		t.Error("a successful probe should mark the camera online")
	}
	if source.calls != 1 {
		t.Errorf("camera was queried %d times, want 1", source.calls)
	}
}

func TestProbeRecordsAnOfflineCamera(t *testing.T) {
	source := &stubSource{err: errors.New("connection refused")}
	m, _, store := newTestMonitor(t, source)

	m.Probe(context.Background())

	data := store.Get()
	if data.CameraOnline {
		t.Error("a failed probe should mark the camera offline")
	}
	if data.LastProbeError == "" {
		t.Error("the probe error should be recorded")
	}
	if !data.LastProbeOK.IsZero() {
		t.Error("a failed probe must not record a success time")
	}
}

func TestProbeFindsTheNewestArchivedFrame(t *testing.T) {
	m, cfg, store := newTestMonitor(t, nil)

	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now().Add(-2 * time.Minute)
	writeArchiveFrame(t, cfg.Sync.ArchiveDir, "2026-09-05", "snapshot_2026-09-05-05-00-00.jpg", old)
	writeArchiveFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-14-00-00.jpg", recent)

	m.Probe(context.Background())

	data := store.Get()
	if data.ArchiveNewest != "snapshot_2026-09-07-14-00-00.jpg" {
		t.Errorf("ArchiveNewest = %q", data.ArchiveNewest)
	}
	if data.ArchiveNewestAt.Before(recent.Add(-time.Second)) {
		t.Errorf("ArchiveNewestAt = %v, want about %v", data.ArchiveNewestAt, recent)
	}
}

// The archive holds a decade of images; finding the newest must not depend on
// walking all of it.
func TestNewestFrameDescendsOnlyTheNewestPath(t *testing.T) {
	m, cfg, _ := newTestMonitor(t, nil)
	_ = m

	base := time.Now().Add(-time.Hour)
	for _, day := range []string{"2019-01-01", "2023-06-15", "2026-09-07"} {
		writeArchiveFrame(t, cfg.Sync.ArchiveDir, day, "snapshot_"+day+"-05-00-00.jpg", base)
	}

	path, _, err := NewestFrame(cfg)
	if err != nil {
		t.Fatalf("NewestFrame: %v", err)
	}
	if filepath.Base(path) != "snapshot_2026-09-07-05-00-00.jpg" {
		t.Errorf("got %q, want the 2026 frame", filepath.Base(path))
	}
}

func TestNewestFrameReportsAnEmptyArchive(t *testing.T) {
	_, cfg, _ := newTestMonitor(t, nil)

	if _, _, err := NewestFrame(cfg); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("expected ErrNoArchive, got %v", err)
	}
}

func TestRunProbesImmediatelyAndStopsOnCancel(t *testing.T) {
	source := &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}}
	m, _, store := newTestMonitor(t, source)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for store.Get().LastProbeAt.IsZero() {
		select {
		case <-deadline:
			t.Fatal("the first probe should happen immediately, not after a full interval")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}

// NAS shares routinely contain directories like these, and several of them sort
// above a four digit year in byte order. Committing to the lexically greatest
// directory made a full archive look empty, which the health check read as a
// failure and restarted the container over.
func TestNewestFrameIgnoresNonDateDirectories(t *testing.T) {
	_, cfg, _ := newTestMonitor(t, nil)

	for _, noise := range []string{"@eaDir", "#recycle", ".snapshot", "lost+found", "zzz"} {
		if err := os.MkdirAll(filepath.Join(cfg.Sync.ArchiveDir, noise, "junk"), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", noise, err)
		}
	}
	want := time.Now().Add(-time.Minute)
	writeArchiveFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-17-00-00.jpg", want)

	path, at, err := NewestFrame(cfg)
	if err != nil {
		t.Fatalf("NewestFrame: %v", err)
	}
	if filepath.Base(path) != "snapshot_2026-09-07-17-00-00.jpg" {
		t.Errorf("found %q", filepath.Base(path))
	}
	if at.Before(want.Add(-time.Second)) {
		t.Errorf("modification time = %v, want about %v", at, want)
	}
}

// An empty directory for a newer day must not hide the images of an older one,
// which is what happens when the scan commits to the newest branch.
func TestNewestFrameBacktracksPastEmptyDirectories(t *testing.T) {
	_, cfg, _ := newTestMonitor(t, nil)

	// Today's directory exists but is still empty, as it is every morning
	// before the first capture.
	for _, empty := range []string{"2026-09-10", "2026-09-09", "2026-09-08"} {
		dir := filepath.Join(cfg.Sync.ArchiveDir, "2026", "2026-09", empty)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	want := time.Now().Add(-2 * time.Hour)
	writeArchiveFrame(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_2026-09-07-21-55-00.jpg", want)

	path, _, err := NewestFrame(cfg)
	if err != nil {
		t.Fatalf("NewestFrame should have backtracked to the last day with images: %v", err)
	}
	if filepath.Base(path) != "snapshot_2026-09-07-21-55-00.jpg" {
		t.Errorf("found %q", filepath.Base(path))
	}
}

// An empty month or year should not stop the search either.
func TestNewestFrameBacktracksAcrossMonthsAndYears(t *testing.T) {
	_, cfg, _ := newTestMonitor(t, nil)

	if err := os.MkdirAll(filepath.Join(cfg.Sync.ArchiveDir, "2027", "2027-01", "2027-01-01"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Sync.ArchiveDir, "2026", "2026-12"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeArchiveFrame(t, cfg.Sync.ArchiveDir, "2026-11-30",
		"snapshot_2026-11-30-12-00-00.jpg", time.Now().Add(-time.Hour))

	path, _, err := NewestFrame(cfg)
	if err != nil {
		t.Fatalf("NewestFrame: %v", err)
	}
	if filepath.Base(path) != "snapshot_2026-11-30-12-00-00.jpg" {
		t.Errorf("found %q", filepath.Base(path))
	}
}

func TestNewestFrameStillReportsAGenuinelyEmptyArchive(t *testing.T) {
	_, cfg, _ := newTestMonitor(t, nil)
	if err := os.MkdirAll(filepath.Join(cfg.Sync.ArchiveDir, "@eaDir"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := NewestFrame(cfg); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("expected ErrNoArchive, got %v", err)
	}
}
