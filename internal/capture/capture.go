// Package capture fetches a frame and stores it in the spool directory using
// the same layout the Raspberry Pi script produced:
//
//	<spool>/YYYY/YYYY-MM/YYYY-MM-DD/<prefix>YYYY-MM-DD-HH-MM-SS.jpg
//
// Writes are atomic: the image is written to a temporary file, flushed, and
// only then renamed into place. A concurrent sync therefore never observes a
// partially written frame.
package capture

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/camera"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// Capturer stores frames from a camera source into the spool.
type Capturer struct {
	cfg    *config.Config
	source camera.Source
	store  *state.Store
	log    *slog.Logger
}

// New returns a Capturer.
func New(cfg *config.Config, source camera.Source, store *state.Store, log *slog.Logger) *Capturer {
	return &Capturer{cfg: cfg, source: source, store: store, log: log}
}

// Result describes a stored frame.
type Result struct {
	Path       string
	RelPath    string
	Filename   string
	Bytes      int64
	CapturedAt time.Time
}

// RelDir returns the archive-relative directory for an instant, e.g.
// "2026/2026-09/2026-09-07".
func RelDir(t time.Time) string {
	return filepath.Join(t.Format("2006"), t.Format("2006-01"), t.Format("2006-01-02"))
}

// Filename returns the file name for an instant, e.g.
// "snapshot_2026-09-07-13-45-00.jpg".
func Filename(prefix string, t time.Time) string {
	return prefix + t.Format("2006-01-02-15-04-05") + ".jpg"
}

// Capture fetches one frame and writes it to the spool. The returned error is
// already recorded in the state store.
func (c *Capturer) Capture(ctx context.Context) (*Result, error) {
	now := time.Now().In(c.cfg.Location)

	c.store.Update(func(d *state.Data) { d.LastCaptureAttempt = now })

	data, err := c.source.Snapshot(ctx)
	if err != nil {
		c.store.Update(func(d *state.Data) {
			d.LastCaptureError = err.Error()
			d.ConsecutiveFailures++
			d.CapturesFailed++
		})
		return nil, err
	}

	relDir := RelDir(now)
	filename := Filename(c.cfg.Capture.FilenamePrefix, now)
	dir := filepath.Join(c.cfg.Capture.SpoolDir, relDir)

	if err := os.MkdirAll(dir, 0o750); err != nil {
		err = fmt.Errorf("creating spool directory: %w", err)
		c.recordFailure(err)
		return nil, err
	}

	path := filepath.Join(dir, filename)
	if err := writeAtomic(path, data); err != nil {
		err = fmt.Errorf("writing %s: %w", path, err)
		c.recordFailure(err)
		return nil, err
	}

	// Mirror the frame into the state directory so the web UI can serve the
	// most recent image even after the original has been moved to the archive.
	if err := writeAtomic(c.store.LatestImagePath(), data); err != nil {
		// Not fatal: the frame itself is safely stored.
		c.log.Warn("could not update latest image mirror", "error", err)
	}

	result := &Result{
		Path:       path,
		RelPath:    filepath.Join(relDir, filename),
		Filename:   filename,
		Bytes:      int64(len(data)),
		CapturedAt: now,
	}

	c.store.Update(func(d *state.Data) {
		d.LastCaptureSuccess = now
		d.LastCaptureError = ""
		d.ConsecutiveFailures = 0
		d.CapturesOK++
		d.Latest = &state.Snapshot{
			Filename:   result.Filename,
			RelPath:    result.RelPath,
			CapturedAt: result.CapturedAt,
			Bytes:      result.Bytes,
		}
	})
	return result, nil
}

// Live fetches a frame for display only. Nothing is written to disk, so the
// archive keeps exactly one image per configured interval and timelapse spacing
// stays uniform.
func (c *Capturer) Live(ctx context.Context) ([]byte, error) {
	return c.source.Snapshot(ctx)
}

func (c *Capturer) recordFailure(err error) {
	c.store.Update(func(d *state.Data) {
		d.LastCaptureError = err.Error()
		d.ConsecutiveFailures++
		d.CapturesFailed++
	})
}

// writeAtomic writes data to a temporary file in the same directory, fsyncs it,
// and renames it into place. The parent directory is fsynced too so the rename
// survives a power loss.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Harmless when the rename already succeeded.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Directory fsync is not supported on every filesystem; a failure here does
	// not mean the file is lost.
	_ = f.Sync()
	return nil
}
