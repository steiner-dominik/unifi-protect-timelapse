package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv restores the whole environment after the test.
//
// LoadAddonOptions calls os.Setenv itself, which t.Setenv cannot track, so
// without this a test's options would leak into every test that runs after it.
func isolateEnv(t *testing.T) {
	t.Helper()
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, entry := range saved {
			if key, value, ok := strings.Cut(entry, "="); ok {
				_ = os.Setenv(key, value)
			}
		}
	})
}

func writeOptions(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing options: %v", err)
	}
	return path
}

// Option keys map to environment variables by uppercasing, so the add-on schema
// and the container's environment stay in step without a translation table.
func TestAddonOptionsMapToEnvironment(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{
		"camera_source": "protect",
		"camera_fallback_source": "snapshot",
		"protect_camera_id": "abc123",
		"capture_enabled": false,
		"capture_interval": "10m",
		"video_fps": 24,
		"monitor_interval": "5m"
	}`)

	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}

	want := map[string]string{
		"CAMERA_SOURCE":          "protect",
		"CAMERA_FALLBACK_SOURCE": "snapshot",
		"PROTECT_CAMERA_ID":      "abc123",
		"CAPTURE_ENABLED":        "false",
		"CAPTURE_INTERVAL":       "10m",
		// A JSON number must not arrive as "24.000000".
		"VIDEO_FPS":        "24",
		"MONITOR_INTERVAL": "5m",
	}
	for key, expected := range want {
		if got := os.Getenv(key); got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
}

func TestAddonAliasesAreApplied(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{
		"auth_mode": "ingress",
		"archive_path": "/media/timelapse",
		"language": "de",
		"entity_prefix": "attic"
	}`)

	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}

	want := map[string]string{
		"WEB_AUTH_MODE":        "ingress",
		"ARCHIVE_DIR":          "/media/timelapse",
		"WEB_DEFAULT_LANGUAGE": "de",
		"HA_ENTITY_PREFIX":     "attic",
	}
	for key, expected := range want {
		if got := os.Getenv(key); got != expected {
			t.Errorf("%s = %q, want %q", key, got, expected)
		}
	}
}

// The container's own environment stays authoritative, so a compose deployment
// is never surprised by a stray options file.
func TestEnvironmentWinsOverAddonOptions(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{"camera_source": "protect"}`)
	t.Setenv("CAMERA_SOURCE", "snapshot")

	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}
	if got := os.Getenv("CAMERA_SOURCE"); got != "snapshot" {
		t.Errorf("CAMERA_SOURCE = %q, want the environment's value", got)
	}
}

// An empty option means "not configured" and must not shadow a useful default.
func TestEmptyOptionsAreIgnored(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{"camera_fallback_source": "", "protect_api_key": "   "}`)
	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}
	if _, set := os.LookupEnv("CAMERA_FALLBACK_SOURCE"); set {
		t.Error("an empty option should not be applied")
	}
	if _, set := os.LookupEnv("PROTECT_API_KEY"); set {
		t.Error("a blank option should not be applied")
	}
}

// Not running as an add-on is the normal case, not an error.
func TestMissingOptionsFileIsNotAnError(t *testing.T) {
	isolateEnv(t)
	if err := LoadAddonOptions(filepath.Join(t.TempDir(), "absent.json")); err != nil {
		t.Fatalf("a missing options file should be tolerated, got %v", err)
	}
}

func TestMalformedOptionsFileIsReported(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{not json`)
	if err := LoadAddonOptions(path); err == nil {
		t.Fatal("expected a parse error to be reported")
	}
}

// Nested structures are skipped rather than mangled; the schema is flat.
func TestNestedOptionsAreSkipped(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{"email": {"from": "a@b.c"}, "list": [1, 2], "log_level": "debug"}`)
	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}
	if _, set := os.LookupEnv("EMAIL"); set {
		t.Error("a nested object should not be applied")
	}
	if got := os.Getenv("LOG_LEVEL"); got != "debug" {
		t.Errorf("LOG_LEVEL = %q, want debug", got)
	}
}

// An add-on's whole configuration should produce a valid running config.
func TestAddonOptionsProduceAWorkingWatchdogConfig(t *testing.T) {
	isolateEnv(t)
	path := writeOptions(t, `{
		"capture_enabled": false,
		"sync_mode": "off",
		"camera_source": "snapshot",
		"camera_snapshot_url": "http://camera.example.lan/snap.jpeg",
		"archive_path": "/media/timelapse",
		"auth_mode": "ingress",
		"monitor_interval": "5m",
		"archive_max_age": "15m"
	}`)

	if err := LoadAddonOptions(path); err != nil {
		t.Fatalf("LoadAddonOptions: %v", err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("the add-on's options should produce a valid configuration: %v", err)
	}

	if cfg.Capture.Enabled {
		t.Error("capture should be disabled")
	}
	if !cfg.Monitor.Enabled {
		t.Error("the watchdog should default on when capture is off")
	}
	if cfg.Web.AuthMode != AuthIngress {
		t.Errorf("AuthMode = %q, want ingress", cfg.Web.AuthMode)
	}
	if cfg.Sync.ArchiveDir != "/media/timelapse" {
		t.Errorf("ArchiveDir = %q", cfg.Sync.ArchiveDir)
	}
}
