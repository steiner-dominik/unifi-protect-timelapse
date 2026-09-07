package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/capture"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/schedule"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
)

func newTestServer(t *testing.T, token string) (*Server, *config.Config) {
	t.Helper()

	cfg := &config.Config{
		Location:       time.UTC,
		MetricsEnabled: true,
		Camera: config.Camera{
			Kind:            config.SourceProtect,
			ProtectHost:     "https://console.example.test",
			ProtectAPIKey:   "super-secret-key",
			ProtectCameraID: "abc123",
			Timeout:         time.Second,
			Retries:         1,
		},
		Capture:  config.Capture{Enabled: true, Interval: 5 * time.Minute, SpoolDir: t.TempDir(), FilenamePrefix: "snapshot_"},
		Schedule: config.Schedule{Mode: config.ScheduleFixed, StartHour: 0, EndHour: 23},
		Sync: config.Sync{
			Mode: config.SyncOpportunistic, ArchiveDir: t.TempDir(), Sentinel: ".nas", At: "23:00",
		},
		Web: config.Web{
			Enabled: true, Addr: ":0", AuthToken: token,
			DefaultLanguage: "en", ArchiveEnabled: true, LivePreview: true,
			LiveMinInterval: time.Second,
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("opening state: %v", err)
	}

	server, err := New(Options{
		Config:    cfg,
		State:     store,
		Capturer:  capture.New(cfg, nil, store, log),
		Syncer:    syncer.New(cfg, store, log),
		Scheduler: schedule.New(cfg),
		Log:       log,
		Version:   "test-1.2.3",
	})
	if err != nil {
		t.Fatalf("building server: %v", err)
	}
	return server, cfg
}

func TestSecurityHeadersArePresent(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := recorder.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
	// The strict policy only holds because the page has no inline script.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP must not relax script execution: %s", csp)
	}
}

func TestAuthRequiredWhenTokenConfigured(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	handler := server.Handler()

	// No credentials.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request = %d, want 401", recorder.Code)
	}

	// Wrong bearer token.
	recorder = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", recorder.Code)
	}

	// Correct bearer token.
	recorder = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Errorf("valid token = %d, want 200", recorder.Code)
	}
}

// The health check has to stay reachable for the container runtime, which
// carries no session.
func TestHealthAndMetricsBypassAuth(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	handler := server.Handler()

	for _, path := range []string{"/healthz", "/metrics"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusUnauthorized {
			t.Errorf("%s must not require authentication", path)
		}
	}
}

// A bookmarked ?token= URL exchanges itself for a cookie and redirects, so the
// token does not linger in the address bar or in history.
func TestTokenQueryBootstrapsCookie(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/?token=s3cret", nil))

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got %d", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); strings.Contains(location, "token") {
		t.Errorf("the token must be stripped from the redirect target, got %q", location)
	}

	var found *http.Cookie
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == authCookie {
			found = cookie
		}
	}
	if found == nil {
		t.Fatal("no session cookie was set")
	}
	if !found.HttpOnly {
		t.Error("the session cookie must be HttpOnly")
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Error("the session cookie must be SameSite=Strict")
	}
}

// The debug view is the most likely place for a secret to leak by accident.
func TestStatusNeverExposesSecrets(t *testing.T) {
	server, _ := newTestServer(t, "s3cret")
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	server.Handler().ServeHTTP(recorder, req)

	body := recorder.Body.String()
	for _, secret := range []string{"super-secret-key", "s3cret"} {
		if strings.Contains(body, secret) {
			t.Errorf("the status response leaked %q:\n%s", secret, body)
		}
	}
	// It should still report that a key is configured.
	if !strings.Contains(body, `"protectKeySet":true`) {
		t.Error("the status response should report that the API key is set")
	}
}

// Assets are cached for a year only when the version matches, which is what
// makes a new release invalidate every cached file.
func TestStaticCachingIsVersionScoped(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/app.css?v=test-1.2.3", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("versioned asset = %d", recorder.Code)
	}
	if cc := recorder.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("versioned asset should be immutable, got %q", cc)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/app.css?v=stale", nil))
	if cc := recorder.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("an asset from another version must be revalidated, got %q", cc)
	}

	// The shell itself must always revalidate, or it would keep pointing at the
	// previous release's assets.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := recorder.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", cc)
	}
}

func TestVersionIsSubstitutedIntoAssets(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	for _, path := range []string{"/", "/sw.js", "/manifest.webmanifest"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		if strings.Contains(body, "__VERSION__") {
			t.Errorf("%s still contains the version placeholder", path)
		}
		if !strings.Contains(body, "test-1.2.3") {
			t.Errorf("%s does not carry the build version", path)
		}
	}
}

func TestStaticRejectsUnknownTypesAndTraversal(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	for _, path := range []string{
		"/static/../../etc/passwd",
		"/static/i18n/../../go.mod",
		"/static/nonexistent.exe",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusOK {
			t.Errorf("%s should not be served", path)
		}
	}
}

func TestTranslationsAreServedForEveryListedLanguage(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	if len(server.langs) < 2 {
		t.Fatalf("expected at least English and German, got %v", server.langs)
	}
	for _, lang := range server.langs {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/i18n/"+lang, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("language %q = %d", lang, recorder.Code)
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/i18n/../../etc/passwd", nil))
	if recorder.Code == http.StatusOK {
		t.Error("an unknown language must not be served")
	}
}

func TestLatestImageIsServedFromStateMirror(t *testing.T) {
	server, _ := newTestServer(t, "")
	handler := server.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/latest.jpg", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("with no capture yet the endpoint should 404, got %d", recorder.Code)
	}

	if err := os.WriteFile(server.store.LatestImagePath(), []byte{0xFF, 0xD8, 0xFF, 0x00}, 0o600); err != nil {
		t.Fatalf("writing mirror: %v", err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/latest.jpg", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected the mirrored image, got %d", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestArchiveEndpointsServeStoredFrames(t *testing.T) {
	server, cfg := newTestServer(t, "")
	handler := server.Handler()

	dir := filepath.Join(cfg.Sync.ArchiveDir, "2026", "2026-09", "2026-09-07")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	name := "snapshot_2026-09-07-05-00-00.jpg"
	if err := os.WriteFile(filepath.Join(dir, name), []byte{0xFF, 0xD8, 0xFF, 0x00}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, tc := range []struct{ path, want string }{
		{"/api/archive/years", `"2026"`},
		{"/api/archive/years/2026/months", `"2026-09"`},
		{"/api/archive/months/2026-09/days", `"2026-09-07"`},
		{"/api/archive/days/2026-09-07/frames", name},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d", tc.path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), tc.want) {
			t.Errorf("%s did not contain %q: %s", tc.path, tc.want, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/archive/days/2026-09-07/frames/"+name, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("serving a frame = %d", recorder.Code)
	}
}

func TestMetricsExposeExpectedSeries(t *testing.T) {
	server, _ := newTestServer(t, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := recorder.Body.String()
	for _, series := range []string{
		"timelapse_build_info",
		"timelapse_captures_total",
		"timelapse_last_capture_success_timestamp_seconds",
		"timelapse_spool_files",
		"timelapse_archive_available",
	} {
		if !strings.Contains(body, series) {
			t.Errorf("metrics are missing %q", series)
		}
	}
}
