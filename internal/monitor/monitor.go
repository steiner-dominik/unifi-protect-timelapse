// Package monitor implements the watchdog: it proves the camera is reachable
// and that images keep landing in the archive, without writing anything itself.
//
// This is what makes a non-capturing deployment useful. The Home Assistant
// add-on runs with capture and sync disabled and does nothing but this: probe
// the camera, look at how fresh the newest archived image is, and report both.
package monitor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/camera"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// ErrNoArchive reports that no archived image exists yet.
var ErrNoArchive = errors.New("no archived image found")

// Monitor probes the camera and the archive.
type Monitor struct {
	cfg    *config.Config
	source camera.Source
	store  *state.Store
	log    *slog.Logger
}

// New returns a Monitor. source may be nil, in which case only the archive is
// checked.
func New(cfg *config.Config, source camera.Source, store *state.Store, log *slog.Logger) *Monitor {
	return &Monitor{cfg: cfg, source: source, store: store, log: log}
}

// Run probes on the configured interval until ctx is cancelled. The first probe
// happens immediately, so a freshly started container reports something useful
// straight away rather than after a full interval of nothing.
func (m *Monitor) Run(ctx context.Context) error {
	m.Probe(ctx)

	ticker := time.NewTicker(m.cfg.Monitor.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.Probe(ctx)
		}
	}
}

// Probe runs one round of checks.
func (m *Monitor) Probe(ctx context.Context) {
	m.probeCamera(ctx)
	m.probeArchive()
}

// probeCamera fetches a frame and throws it away. Nothing is written, so a
// watchdog deployment cannot disturb the capture interval of the real one.
func (m *Monitor) probeCamera(ctx context.Context) {
	if m.source == nil {
		return
	}

	timeout := m.cfg.Camera.Timeout * time.Duration(max(m.cfg.Camera.Retries, 1))
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := time.Now()
	_, err := m.source.Snapshot(probeCtx)
	if ctx.Err() != nil {
		return
	}

	m.store.Update(func(d *state.Data) {
		d.LastProbeAt = now
		d.CameraLastUsed = camera.LastUsed(m.source)
		if err != nil {
			d.CameraOnline = false
			d.LastProbeError = err.Error()
			return
		}
		d.CameraOnline = true
		d.LastProbeOK = now
		d.LastProbeError = ""
	})

	if err != nil {
		m.log.Warn("camera probe failed", "error", err)
		return
	}
	m.log.Debug("camera probe ok")
}

// probeArchive records the newest image present, so staleness can be judged
// without the monitor having captured anything.
func (m *Monitor) probeArchive() {
	path, modTime, err := NewestFrame(m.cfg)
	if err != nil {
		return
	}
	m.store.Update(func(d *state.Data) {
		d.ArchiveNewestAt = modTime
		d.ArchiveNewest = filepath.Base(path)
	})
}

// Roots lists the directories that may hold images, archive first. A watchdog
// deployment has sync switched off but still points ARCHIVE_DIR at the archive
// it is watching, so the archive is included whenever it is configured.
func Roots(cfg *config.Config) []string {
	var roots []string
	if cfg.Sync.ArchiveDir != "" {
		roots = append(roots, cfg.Sync.ArchiveDir)
	}
	if cfg.Capture.SpoolDir != "" {
		roots = append(roots, cfg.Capture.SpoolDir)
	}
	return roots
}

// NewestFrame returns the path and modification time of the most recent image
// across the configured roots.
//
// It descends into only the newest year, then month, then day rather than
// walking the tree, which keeps it cheap over NFS even with a decade of images.
func NewestFrame(cfg *config.Config) (string, time.Time, error) {
	var (
		bestPath string
		bestAt   time.Time
	)

	for _, root := range Roots(cfg) {
		dir := root
		// Three levels: YYYY / YYYY-MM / YYYY-MM-DD.
		for range 3 {
			sub, ok := newestSubdir(dir)
			if !ok {
				break
			}
			dir = sub
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".jpg") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(bestAt) {
				bestAt = info.ModTime()
				bestPath = filepath.Join(dir, entry.Name())
			}
		}
	}

	if bestPath == "" {
		return "", time.Time{}, ErrNoArchive
	}
	return bestPath, bestAt, nil
}

// newestSubdir returns the lexically greatest subdirectory, which for the
// zero-padded YYYY / YYYY-MM / YYYY-MM-DD layout is also the most recent.
func newestSubdir(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}

	best := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if name := entry.Name(); name > best {
			best = name
		}
	}
	if best == "" {
		return "", false
	}
	return filepath.Join(dir, best), true
}
