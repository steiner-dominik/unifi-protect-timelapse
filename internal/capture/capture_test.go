package capture

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

type stubSource struct {
	data []byte
	err  error
}

func (s *stubSource) Snapshot(context.Context) ([]byte, error) { return s.data, s.err }
func (s *stubSource) Describe() string                         { return "stub" }

func newTestCapturer(t *testing.T, source *stubSource) (*Capturer, *config.Config, *state.Store) {
	t.Helper()

	cfg := &config.Config{
		Location: time.UTC,
		Capture: config.Capture{
			Enabled: true, Interval: 5 * time.Minute,
			FilenamePrefix: "snapshot_", SpoolDir: t.TempDir(),
		},
	}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, source, store, log), cfg, store
}

// The directory layout must match what the Raspberry Pi produced, so the
// existing archive stays uniform across the switchover.
func TestCaptureUsesPiCompatibleLayout(t *testing.T) {
	capturer, cfg, _ := newTestCapturer(t, &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}})

	result, err := capturer.Capture(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}

	now := time.Now().In(cfg.Location)
	wantDir := filepath.Join(now.Format("2006"), now.Format("2006-01"), now.Format("2006-01-02"))
	if dir := filepath.Dir(result.RelPath); dir != wantDir {
		t.Errorf("directory = %q, want %q", dir, wantDir)
	}
	if !strings.HasPrefix(result.Filename, "snapshot_") || !strings.HasSuffix(result.Filename, ".jpg") {
		t.Errorf("unexpected filename %q", result.Filename)
	}
	// snapshot_ + YYYY-MM-DD-HH-MM-SS + .jpg
	if len(result.Filename) != len("snapshot_")+19+4 {
		t.Errorf("filename %q does not match the expected timestamp format", result.Filename)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Errorf("captured file is missing: %v", err)
	}
}

// Nothing with the temporary prefix may survive a capture, because the syncer
// identifies in-flight writes by that prefix.
func TestCaptureLeavesNoTemporaryFiles(t *testing.T) {
	capturer, cfg, _ := newTestCapturer(t, &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}})

	if _, err := capturer.Capture(context.Background()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	var leftovers []string
	err := filepath.Walk(cfg.Capture.SpoolDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasPrefix(info.Name(), ".tmp-") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking spool: %v", err)
	}
	if len(leftovers) > 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestCaptureMirrorsLatestImage(t *testing.T) {
	want := []byte{0xFF, 0xD8, 0xFF, 0x42}
	capturer, _, store := newTestCapturer(t, &stubSource{data: want})

	if _, err := capturer.Capture(context.Background()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	got, err := os.ReadFile(store.LatestImagePath())
	if err != nil {
		t.Fatalf("reading the mirror: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the mirrored image does not match the captured one")
	}
}

func TestCaptureRecordsFailures(t *testing.T) {
	capturer, _, store := newTestCapturer(t, &stubSource{err: errors.New("camera unreachable")})

	for range 3 {
		if _, err := capturer.Capture(context.Background()); err == nil {
			t.Fatal("expected the capture to fail")
		}
	}

	data := store.Get()
	if data.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d, want 3", data.ConsecutiveFailures)
	}
	if data.CapturesFailed != 3 {
		t.Errorf("CapturesFailed = %d, want 3", data.CapturesFailed)
	}
	if !strings.Contains(data.LastCaptureError, "camera unreachable") {
		t.Errorf("LastCaptureError = %q", data.LastCaptureError)
	}
	if !data.LastCaptureSuccess.IsZero() {
		t.Error("a failing capture must not record a success")
	}
}

func TestCaptureResetsFailureCounterOnSuccess(t *testing.T) {
	source := &stubSource{err: errors.New("boom")}
	capturer, _, store := newTestCapturer(t, source)

	_, _ = capturer.Capture(context.Background())
	if store.Get().ConsecutiveFailures != 1 {
		t.Fatal("expected one recorded failure")
	}

	source.err = nil
	source.data = []byte{0xFF, 0xD8, 0xFF, 0x01}
	if _, err := capturer.Capture(context.Background()); err != nil {
		t.Fatalf("capture: %v", err)
	}

	data := store.Get()
	if data.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", data.ConsecutiveFailures)
	}
	if data.LastCaptureError != "" {
		t.Errorf("LastCaptureError should be cleared, got %q", data.LastCaptureError)
	}
}

// Live previews exist so the browser can look at the camera without changing
// the archive; they must not write anything.
func TestLiveDoesNotTouchTheSpool(t *testing.T) {
	capturer, cfg, store := newTestCapturer(t, &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}})

	if _, err := capturer.Live(context.Background()); err != nil {
		t.Fatalf("live: %v", err)
	}

	entries, err := os.ReadDir(cfg.Capture.SpoolDir)
	if err != nil {
		t.Fatalf("reading spool: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a live preview must not write to the spool, found %d entries", len(entries))
	}
	if store.Get().CapturesOK != 0 {
		t.Error("a live preview must not count as a capture")
	}
}

// A camera that answers with the same bytes forever looks healthy by every
// other measure, which is exactly why this check exists.
func TestFrozenFrameDetection(t *testing.T) {
	frame := []byte{0xFF, 0xD8, 0xFF, 0x01, 0x02, 0x03}
	source := &stubSource{data: frame}
	capturer, cfg, store := newTestCapturer(t, source)
	cfg.Monitor.FrozenThreshold = 3

	// The first frame has no predecessor, so identical counting starts after it.
	for i := range 3 {
		result, err := capturer.Capture(context.Background())
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		if result.Frozen {
			t.Fatalf("capture %d should not be flagged frozen yet (count %d)", i, result.IdenticalCount)
		}
	}

	result, err := capturer.Capture(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !result.Frozen {
		t.Fatalf("expected the fourth identical frame to be flagged, count was %d", result.IdenticalCount)
	}
	if !store.Get().FrameFrozen {
		t.Error("the frozen state should be persisted")
	}
}

func TestFrozenDetectionResetsOnANewFrame(t *testing.T) {
	source := &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}}
	capturer, cfg, store := newTestCapturer(t, source)
	cfg.Monitor.FrozenThreshold = 2

	for range 4 {
		if _, err := capturer.Capture(context.Background()); err != nil {
			t.Fatalf("capture: %v", err)
		}
	}
	if !store.Get().FrameFrozen {
		t.Fatal("expected the camera to be flagged frozen")
	}

	// A genuinely new frame clears the condition.
	source.data = []byte{0xFF, 0xD8, 0xFF, 0x99}
	result, err := capturer.Capture(context.Background())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if result.Frozen || result.IdenticalCount != 0 {
		t.Errorf("a new frame should clear the frozen state, got frozen=%v count=%d",
			result.Frozen, result.IdenticalCount)
	}
	if store.Get().FrameFrozen {
		t.Error("the persisted frozen state should be cleared")
	}
}

func TestFrozenDetectionCanBeDisabled(t *testing.T) {
	source := &stubSource{data: []byte{0xFF, 0xD8, 0xFF, 0x01}}
	capturer, cfg, store := newTestCapturer(t, source)
	cfg.Monitor.FrozenThreshold = 0

	for range 5 {
		if _, err := capturer.Capture(context.Background()); err != nil {
			t.Fatalf("capture: %v", err)
		}
	}
	if store.Get().FrameFrozen {
		t.Error("frozen detection should be off when the threshold is zero")
	}
}
