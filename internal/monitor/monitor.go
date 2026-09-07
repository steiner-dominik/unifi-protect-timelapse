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
	"regexp"
	"sort"
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

// Date directory names, so that anything else in the archive is ignored. NAS
// shares routinely contain directories like @eaDir, #recycle or .snapshot, and
// several of those sort above a four digit year in byte order.
var (
	yearDir  = regexp.MustCompile(`^\d{4}$`)
	monthDir = regexp.MustCompile(`^\d{4}-\d{2}$`)
	dayDir   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// levels is the directory pattern at each depth of the archive layout.
var levels = []*regexp.Regexp{yearDir, monthDir, dayDir}

// NewestFrame returns the path and modification time of the most recent image
// across the configured roots.
//
// It walks newest-first and backtracks, rather than committing to the single
// newest directory at each level. Committing meant one empty or unrelated
// directory made the whole archive look empty, which the health check then read
// as a failure.
func NewestFrame(cfg *config.Config) (string, time.Time, error) {
	var (
		bestPath string
		bestAt   time.Time
	)

	for _, root := range Roots(cfg) {
		path, at, ok := newestUnder(root, 0)
		if ok && at.After(bestAt) {
			bestPath, bestAt = path, at
		}
	}

	if bestPath == "" {
		return "", time.Time{}, ErrNoArchive
	}
	return bestPath, bestAt, nil
}

// newestUnder searches dir for the most recent image, descending through the
// date directories newest-first and giving up on a branch that holds nothing.
//
// maxBranches bounds the backtracking: a handful of empty directories is normal
// and worth stepping over, but an archive full of unrelated directories should
// not turn this into a full tree walk on every probe.
func newestUnder(dir string, depth int) (string, time.Time, bool) {
	if depth >= len(levels) {
		return newestImageIn(dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() && levels[depth].MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	// Zero padded date names sort chronologically, so newest is last.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	const maxBranches = 8
	for i, name := range names {
		if i >= maxBranches {
			break
		}
		if path, at, ok := newestUnder(filepath.Join(dir, name), depth+1); ok {
			return path, at, true
		}
	}
	return "", time.Time{}, false
}

// newestImageIn returns the most recently modified JPEG directly inside dir.
func newestImageIn(dir string) (string, time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}

	var (
		bestPath string
		bestAt   time.Time
	)
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
	return bestPath, bestAt, bestPath != ""
}
