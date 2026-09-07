package camera

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// jpegBody is a minimal payload that starts with the JPEG SOI marker.
func jpegBody(size int) []byte {
	body := make([]byte, size)
	copy(body, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	return body
}

func testCameraConfig(url string) *config.Config {
	return &config.Config{
		Camera: config.Camera{
			Kind:          config.SourceSnapshot,
			SnapshotURL:   url,
			Timeout:       5 * time.Second,
			Retries:       1,
			MinImageBytes: 1024,
		},
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The original shell script used `curl` without -f, so an HTTP error page was
// archived as a .jpg. Every one of these cases must now be rejected.
func TestSnapshotRejectsNonImageResponses(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http error", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}},
		{"html error page with 200", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>Not found</body></html>"))
		}},
		{"too small", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegBody(10))
		}},
		{"image content type but not a jpeg", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(make([]byte, 4096))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			source, err := New(testCameraConfig(server.URL), discardLogger())
			if err != nil {
				t.Fatalf("building source: %v", err)
			}
			if _, err := source.Snapshot(context.Background()); err == nil {
				t.Fatal("expected the response to be rejected")
			}
		})
	}
}

func TestSnapshotAcceptsValidJPEG(t *testing.T) {
	want := jpegBody(4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(want)
	}))
	defer server.Close()

	source, err := New(testCameraConfig(server.URL), discardLogger())
	if err != nil {
		t.Fatalf("building source: %v", err)
	}
	got, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d bytes, got %d", len(want), len(got))
	}
}

func TestProtectSourceSendsAPIKey(t *testing.T) {
	var gotKey, gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-KEY")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody(4096))
	}))
	defer server.Close()

	cfg := &config.Config{Camera: config.Camera{
		Kind:               config.SourceProtect,
		ProtectHost:        server.URL,
		ProtectAPIKey:      "secret-key",
		ProtectCameraID:    "abc123",
		ProtectHighQuality: true,
		Timeout:            5 * time.Second,
		Retries:            1,
		MinImageBytes:      1024,
	}}

	source, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("building source: %v", err)
	}
	if _, err := source.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if gotKey != "secret-key" {
		t.Errorf("X-API-KEY = %q", gotKey)
	}
	if want := "/proxy/protect/integration/v1/cameras/abc123/snapshot"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotQuery != "highQuality=true" {
		t.Errorf("query = %q", gotQuery)
	}
}

// Describe feeds the debug view, so it must never carry the API key.
func TestDescribeOmitsSecrets(t *testing.T) {
	cfg := &config.Config{Camera: config.Camera{
		Kind:            config.SourceProtect,
		ProtectHost:     "https://console.example.test",
		ProtectAPIKey:   "super-secret-key",
		ProtectCameraID: "abc123",
		Timeout:         time.Second,
		Retries:         1,
	}}
	source, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("building source: %v", err)
	}
	if desc := source.Describe(); strings.Contains(desc, "super-secret-key") {
		t.Fatalf("Describe leaked the API key: %q", desc)
	}
}

func TestRetryingRetriesThenSucceeds(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBody(4096))
	}))
	defer server.Close()

	source, err := New(testCameraConfig(server.URL), discardLogger())
	if err != nil {
		t.Fatalf("building source: %v", err)
	}
	retrying := &Retrying{Source: source, Attempts: 3, Delay: time.Millisecond, Log: discardLogger()}

	if _, err := retrying.Snapshot(context.Background()); err != nil {
		t.Fatalf("expected success on the third attempt, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestRetryingHonoursContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	source, err := New(testCameraConfig(server.URL), discardLogger())
	if err != nil {
		t.Fatalf("building source: %v", err)
	}
	retrying := &Retrying{Source: source, Attempts: 10, Delay: time.Hour, Log: discardLogger()}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := retrying.Snapshot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the context deadline to interrupt the retries, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("retries did not stop promptly, took %s", elapsed)
	}
}
