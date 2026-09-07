// Package syncer moves spooled images to the archive.
//
// The critical safety property is inherited from the original move_to_nas.sh:
// before anything is moved, a sentinel file must be present in the archive
// directory. Without that check an unmounted NFS share looks exactly like an
// empty local directory, and the sync would happily fill the container's own
// filesystem while deleting the originals. If the sentinel is missing the sync
// aborts and the images stay in the spool until the next attempt, which is
// precisely the buffering behaviour this project exists to preserve.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/capture"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// ErrArchiveUnavailable reports that the archive sentinel is missing, meaning
// the target is not mounted.
var ErrArchiveUnavailable = errors.New("archive unavailable: sentinel file not found")

// Syncer moves files from the spool to the archive.
type Syncer struct {
	cfg   *config.Config
	store *state.Store
	log   *slog.Logger

	// mu serialises sync runs so that the opportunistic post-capture sync, the
	// nightly sweep and a manual trigger can never run concurrently.
	mu sync.Mutex
}

// New returns a Syncer.
func New(cfg *config.Config, store *state.Store, log *slog.Logger) *Syncer {
	return &Syncer{cfg: cfg, store: store, log: log}
}

// Report summarises one sync run.
type Report struct {
	Files    int
	Bytes    int64
	Skipped  int
	Duration time.Duration
}

// ArchiveAvailable reports whether the archive is mounted, by checking for the
// sentinel file.
func (s *Syncer) ArchiveAvailable() bool {
	if !s.cfg.SyncEnabled() {
		return false
	}
	info, err := os.Stat(filepath.Join(s.cfg.Sync.ArchiveDir, s.cfg.Sync.Sentinel))
	return err == nil && !info.IsDir()
}

// Run moves every complete frame from the spool to the archive. It is safe to
// call concurrently; overlapping calls are serialised.
func (s *Syncer) Run(ctx context.Context) (*Report, error) {
	if !s.cfg.SyncEnabled() {
		return &Report{}, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	started := time.Now()
	s.store.Update(func(d *state.Data) { d.LastSyncAttempt = started })

	available := s.ArchiveAvailable()
	s.store.Update(func(d *state.Data) { d.ArchiveAvailable = available })
	if !available {
		err := fmt.Errorf("%w: %s missing in %s",
			ErrArchiveUnavailable, s.cfg.Sync.Sentinel, s.cfg.Sync.ArchiveDir)
		s.recordFailure(err)
		return nil, err
	}

	report, err := s.move(ctx)
	if err != nil {
		s.recordFailure(err)
		return report, err
	}

	if s.cfg.Sync.PruneEmptyDirs {
		if err := s.pruneEmptyDirs(); err != nil {
			s.log.Warn("pruning empty spool directories failed", "error", err)
		}
	}

	report.Duration = time.Since(started)
	s.store.Update(func(d *state.Data) {
		d.LastSyncSuccess = time.Now()
		d.LastSyncError = ""
		d.LastSyncFiles = report.Files
		d.LastSyncBytes = report.Bytes
		d.SyncRunsOK++
	})
	return report, nil
}

// move walks the spool and relocates each finished frame.
func (s *Syncer) move(ctx context.Context) (*Report, error) {
	report := &Report{}

	err := filepath.WalkDir(s.cfg.Capture.SpoolDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			return nil
		}
		// Temporary files are in-flight writes; anything that is not a .jpg was
		// not produced by this service and is left alone.
		name := entry.Name()
		if strings.HasPrefix(name, ".tmp-") || !strings.EqualFold(filepath.Ext(name), ".jpg") {
			report.Skipped++
			return nil
		}

		rel, err := filepath.Rel(s.cfg.Capture.SpoolDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(s.cfg.Sync.ArchiveDir, rel)

		size, err := moveFile(path, target)
		if err != nil {
			return fmt.Errorf("moving %s: %w", rel, err)
		}
		report.Files++
		report.Bytes += size
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return report, err
	}
	return report, nil
}

// moveFile relocates src to dst, falling back to copy-and-remove when the two
// paths are on different filesystems, which is the normal case for a local
// spool and an NFS archive. The destination is written to a temporary name and
// renamed, so an interrupted transfer never leaves a truncated file in the
// archive, and the source is only removed once the destination is durable.
func moveFile(src, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}

	if err := os.Rename(src, dst); err == nil {
		info, statErr := os.Stat(dst)
		if statErr != nil {
			return 0, nil
		}
		return info.Size(), nil
	}

	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	written, err := io.Copy(tmp, in)
	if err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return 0, err
	}
	if err := in.Close(); err != nil {
		return 0, err
	}
	if err := os.Remove(src); err != nil {
		return written, fmt.Errorf("destination written but source not removed: %w", err)
	}
	return written, nil
}

// pruneEmptyDirs removes empty directories left behind in the spool, skipping
// the tree for the current day so that a directory in active use is never
// deleted out from under a capture.
func (s *Syncer) pruneEmptyDirs() error {
	today := capture.RelDir(time.Now().In(s.cfg.Location))
	root := s.cfg.Capture.SpoolDir

	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	// Deepest first, so a parent becomes empty once its children are gone.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	for _, dir := range dirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		if rel == today || strings.HasPrefix(today, rel+string(filepath.Separator)) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		if err := os.Remove(dir); err != nil {
			s.log.Debug("could not remove empty directory", "dir", dir, "error", err)
		}
	}
	return nil
}

func (s *Syncer) recordFailure(err error) {
	s.store.Update(func(d *state.Data) {
		d.LastSyncError = err.Error()
		d.SyncRunsFailed++
	})
}

// SpoolStats reports how much is currently buffered locally.
type SpoolStats struct {
	Files  int   `json:"files"`
	Bytes  int64 `json:"bytes"`
	Oldest int64 `json:"oldestUnixSeconds"`
}

// Spool returns the current spool statistics.
func (s *Syncer) Spool() SpoolStats {
	var stats SpoolStats
	_ = filepath.WalkDir(s.cfg.Capture.SpoolDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree should not fail the whole stat
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".jpg") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		stats.Files++
		stats.Bytes += info.Size()
		if unix := info.ModTime().Unix(); stats.Oldest == 0 || unix < stats.Oldest {
			stats.Oldest = unix
		}
		return nil
	})
	return stats
}
