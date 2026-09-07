package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// The ingress prefix is echoed into the HTML, so anything that is not a plain
// absolute path has to be discarded rather than reflected.
func TestSanitizeBasePathRejectsHostileValues(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/api/hassio_ingress/abc123", "/api/hassio_ingress/abc123"},
		{"/api/hassio_ingress/abc123/", "/api/hassio_ingress/abc123"},
		{"", ""},
		{"relative/path", ""},
		{"//evil.example.com", ""},
		{"https://evil.example.com", ""},
		{"/a/../../etc", ""},
		{`/x" onload="alert(1)`, ""},
		{"/x<script>", ""},
		{"/x?y=1", ""},
		{"/x#frag", ""},
		{"javascript:alert(1)", ""},
	}

	for _, tc := range cases {
		if got := sanitizeBasePath(tc.in); got != tc.want {
			t.Errorf("sanitizeBasePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIndexCarriesTheIngressPrefix(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	// Served directly: no prefix anywhere.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "__BASE__") {
		t.Error("the base placeholder was not substituted")
	}
	if !strings.Contains(body, `href="/static/app.css`) {
		t.Errorf("expected absolute asset paths when served directly:\n%s", firstLines(body))
	}

	// Behind ingress: every asset reference carries the prefix.
	recorder = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(ingressHeader, "/api/hassio_ingress/tok3n")
	handler.ServeHTTP(recorder, req)

	body = recorder.Body.String()
	if !strings.Contains(body, `data-base="/api/hassio_ingress/tok3n"`) {
		t.Error("the ingress prefix should be published to the frontend")
	}
	if !strings.Contains(body, `href="/api/hassio_ingress/tok3n/static/app.css`) {
		t.Errorf("asset paths should carry the ingress prefix:\n%s", firstLines(body))
	}
	if recorder.Header().Get("Vary") != ingressHeader {
		t.Error("a response that varies by ingress prefix must say so")
	}
}

func TestIngressAuthModeSkipsTheToken(t *testing.T) {
	server, cfg := newTestServer(t, "s3cret")
	cfg.Web.AuthMode = config.AuthIngress

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("under ingress Home Assistant has already authenticated the user, got %d", recorder.Code)
	}
}

func TestLocalAuthModeStillRequiresTheToken(t *testing.T) {
	server, cfg := newTestServer(t, "s3cret")
	cfg.Web.AuthMode = config.AuthLocal

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 in local mode, got %d", recorder.Code)
	}
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 12)
	if len(lines) > 11 {
		lines = lines[:11]
	}
	return strings.Join(lines, "\n")
}

// A watchdog deployment must not sit in "starting up" for twice a capture
// interval it never uses.
func TestStartupGraceIgnoresTheCaptureIntervalWhenNotCapturing(t *testing.T) {
	server, cfg := newTestServer(t, "")
	cfg.Capture.Enabled = false
	cfg.Capture.Interval = time.Hour
	cfg.Monitor.Enabled = true
	cfg.Monitor.Interval = 10 * time.Second

	if grace := server.startupGrace(); grace != 20*time.Second {
		t.Errorf("startupGrace = %s, want 20s (twice the monitor interval)", grace)
	}
}

func TestStartupGraceUsesTheSlowestActiveLoop(t *testing.T) {
	server, cfg := newTestServer(t, "")
	cfg.Capture.Enabled = true
	cfg.Capture.Interval = 5 * time.Minute
	cfg.Monitor.Enabled = true
	cfg.Monitor.Interval = time.Minute

	if grace := server.startupGrace(); grace != 10*time.Minute {
		t.Errorf("startupGrace = %s, want 10m", grace)
	}
}

// With capture disabled, health has to be judged on the camera and the archive.
func TestHealthyInWatchdogMode(t *testing.T) {
	server, cfg := newTestServer(t, "")
	cfg.Capture.Enabled = false
	cfg.Monitor.Enabled = true
	cfg.Monitor.Interval = time.Millisecond
	cfg.Monitor.ArchiveMaxAge = time.Minute

	// An unreachable camera is a problem even though nothing is being captured.
	server.store.Update(func(d *state.Data) {
		d.LastProbeAt = time.Now()
		d.CameraOnline = false
	})
	if healthy, reason := server.Healthy(); healthy {
		t.Errorf("an offline camera should be unhealthy, got %q", reason)
	}

	// Camera fine but the archive has stopped growing.
	server.store.Update(func(d *state.Data) {
		d.CameraOnline = true
		d.ArchiveNewestAt = time.Now().Add(-time.Hour)
	})
	if healthy, reason := server.Healthy(); healthy {
		t.Errorf("a stale archive should be unhealthy, got %q", reason)
	}

	// Both fine.
	server.store.Update(func(d *state.Data) {
		d.ArchiveNewestAt = time.Now()
	})
	if healthy, reason := server.Healthy(); !healthy {
		t.Errorf("expected healthy, got %q", reason)
	}
}
