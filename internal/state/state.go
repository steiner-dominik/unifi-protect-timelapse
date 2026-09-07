// Package state keeps the runtime facts that outlive a single capture: when the
// last snapshot succeeded, what the last sync did, and how many consecutive
// failures we have seen. It is persisted to disk so that the health check can
// run as a separate process (Docker HEALTHCHECK) and so that counters survive a
// container restart.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshot describes the most recently captured image.
type Snapshot struct {
	Filename   string    `json:"filename"`
	RelPath    string    `json:"relPath"`
	CapturedAt time.Time `json:"capturedAt"`
	Bytes      int64     `json:"bytes"`
}

// Data is the serialisable form of the state.
type Data struct {
	LastCaptureAttempt  time.Time `json:"lastCaptureAttempt"`
	LastCaptureSuccess  time.Time `json:"lastCaptureSuccess"`
	LastCaptureError    string    `json:"lastCaptureError"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	CapturesOK          uint64    `json:"capturesOk"`
	CapturesFailed      uint64    `json:"capturesFailed"`

	LastSyncAttempt time.Time `json:"lastSyncAttempt"`
	LastSyncSuccess time.Time `json:"lastSyncSuccess"`
	LastSyncError   string    `json:"lastSyncError"`
	LastSyncFiles   int       `json:"lastSyncFiles"`
	LastSyncBytes   int64     `json:"lastSyncBytes"`
	SyncRunsOK      uint64    `json:"syncRunsOk"`
	SyncRunsFailed  uint64    `json:"syncRunsFailed"`

	ArchiveAvailable bool      `json:"archiveAvailable"`
	Latest           *Snapshot `json:"latest,omitempty"`

	// Monitor results. These are filled by the watchdog loop, which runs
	// without capturing anything, and by capture itself where the two overlap.
	LastProbeAt     time.Time `json:"lastProbeAt"`
	LastProbeOK     time.Time `json:"lastProbeOk"`
	LastProbeError  string    `json:"lastProbeError"`
	CameraOnline    bool      `json:"cameraOnline"`
	ArchiveNewestAt time.Time `json:"archiveNewestAt"`
	ArchiveNewest   string    `json:"archiveNewest"`
	// ArchiveState and ArchiveError say why there are no images, so an empty
	// panel can be told apart from a wrong path.
	ArchiveState string `json:"archiveState"`
	ArchiveError string `json:"archiveError"`

	// Frozen-frame detection. A camera can answer with the same bytes forever;
	// counting identical frames in a row is what catches that.
	LastFrameHash  string `json:"lastFrameHash"`
	IdenticalCount int    `json:"identicalCount"`
	FrameFrozen    bool   `json:"frameFrozen"`

	// CameraLastUsed names the endpoint that served the most recent frame:
	// "primary", "fallback" or "none".
	CameraLastUsed string `json:"cameraLastUsed"`

	// StartedAt is when the running service started. It is persisted so the
	// health check, which runs as a separate process, applies the same startup
	// grace as the HTTP endpoint instead of judging a service that has barely
	// begun.
	StartedAt time.Time `json:"startedAt"`
}

// Store is a concurrency-safe, disk-backed Data.
type Store struct {
	mu   sync.RWMutex
	data Data
	path string
	dir  string
}

// LatestImageName is the file the most recent frame is mirrored to inside the
// state directory, so the web UI can always serve it regardless of whether the
// original has already been moved to the archive.
const LatestImageName = "latest.jpg"

// Open loads the store from dir, creating the directory if needed. A missing or
// unreadable state file is not an error: the service starts with empty state.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating state directory %s: %w", dir, err)
	}
	s := &Store{dir: dir, path: filepath.Join(dir, "state.json")}

	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		// Corrupt state must never stop the service from capturing; the
		// counters simply restart from zero.
		s.data = Data{}
	}
	return s, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// LatestImagePath returns the path of the mirrored most recent frame.
func (s *Store) LatestImagePath() string { return filepath.Join(s.dir, LatestImageName) }

// Get returns a copy of the current state.
func (s *Store) Get() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Update applies fn to the state under lock and persists the result.
func (s *Store) Update(fn func(d *Data)) {
	s.mu.Lock()
	fn(&s.data)
	data := s.data
	s.mu.Unlock()
	// A failed persist is not worth interrupting a capture for; the in-memory
	// state stays authoritative for this process.
	_ = writeJSON(s.path, data)
}

// Load reads the state file without holding a Store, for use by the health
// check subcommand.
func Load(dir string) (Data, error) {
	var d Data
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("parsing state file: %w", err)
	}
	return d, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
