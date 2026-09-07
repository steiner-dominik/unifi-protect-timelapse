package config

import (
	"strings"
	"testing"
)

func setEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for key, value := range values {
		t.Setenv(key, value)
	}
}

// The minimal viable configuration is what the README documents; if this stops
// working the quick-start instructions are wrong.
func TestLoadMinimalSnapshotConfiguration(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "http://camera.example.lan/snap.jpeg",
		"SPOOL_DIR":           "/spool",
		"ARCHIVE_DIR":         "/archive",
		"STATE_DIR":           "/state",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Camera.Kind != SourceSnapshot {
		t.Errorf("default source = %q, want %q", cfg.Camera.Kind, SourceSnapshot)
	}
	if cfg.Sync.Mode != SyncOpportunistic {
		t.Errorf("default sync mode = %q", cfg.Sync.Mode)
	}
	if cfg.Schedule.StartHour != 5 || cfg.Schedule.EndHour != 21 {
		t.Errorf("default window = %d-%d, want 5-21", cfg.Schedule.StartHour, cfg.Schedule.EndHour)
	}
	if cfg.Sync.Sentinel != ".nas" {
		t.Errorf("default sentinel = %q", cfg.Sync.Sentinel)
	}
}

func TestProtectModeRequiresItsSettings(t *testing.T) {
	setEnv(t, map[string]string{"CAMERA_SOURCE": "protect"})

	_, err := Load()
	if err == nil {
		t.Fatal("expected the configuration to be rejected")
	}
	for _, want := range []string{"PROTECT_HOST", "PROTECT_API_KEY", "PROTECT_CAMERA_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

// Every validation error should be reported at once, so a misconfigured deploy
// does not need several restarts to be fixed.
func TestValidationReportsAllProblemsTogether(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "ftp://camera.example.lan/snap.jpeg",
		"ACTIVE_HOURS":        "notanhour",
		"SYNC_AT":             "25:99",
		"CAPTURE_INTERVAL":    "0s",
	})

	_, err := Load()
	if err == nil {
		t.Fatal("expected the configuration to be rejected")
	}
	for _, want := range []string{"CAMERA_SNAPSHOT_URL", "ACTIVE_HOURS", "SYNC_AT", "CAPTURE_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

// Sharing the two directories would make the syncer move files onto themselves.
func TestSpoolAndArchiveMustDiffer(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "http://camera.example.lan/snap.jpeg",
		"SPOOL_DIR":           "/data",
		"ARCHIVE_DIR":         "/data",
	})

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "must not be the same") {
		t.Fatalf("expected the identical directories to be rejected, got %v", err)
	}
}

// An empty sentinel would disable the guard that prevents writing into an
// unmounted mountpoint.
func TestEmptySentinelIsRejected(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "http://camera.example.lan/snap.jpeg",
		"ARCHIVE_SENTINEL":    " ",
	})

	// A blank value falls back to the default rather than disabling the guard.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sync.Sentinel != ".nas" {
		t.Errorf("a blank sentinel should fall back to the default, got %q", cfg.Sync.Sentinel)
	}
}

func TestSolarModeRequiresCoordinates(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "http://camera.example.lan/snap.jpeg",
		"SCHEDULE_MODE":       "solar",
	})

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LATITUDE") {
		t.Fatalf("expected solar mode to require coordinates, got %v", err)
	}
}

func TestActiveHoursParsing(t *testing.T) {
	cases := []struct {
		input      string
		start, end int
		wantErr    bool
	}{
		{"05-21", 5, 21, false},
		{"0-23", 0, 23, false},
		{"22-04", 22, 4, false},
		{"24-25", 0, 0, true},
		{"5", 0, 0, true},
		{"", 0, 0, true},
	}

	for _, tc := range cases {
		start, end, err := parseHours(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseHours(%q) should have failed", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseHours(%q): %v", tc.input, err)
			continue
		}
		if start != tc.start || end != tc.end {
			t.Errorf("parseHours(%q) = %d-%d, want %d-%d", tc.input, start, end, tc.start, tc.end)
		}
	}
}

// The redacted view feeds the web UI, so it must never carry secret material.
func TestPublicViewRedactsSecrets(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SOURCE":     "protect",
		"PROTECT_HOST":      "https://user:password@console.example.lan",
		"PROTECT_API_KEY":   "super-secret-key",
		"PROTECT_CAMERA_ID": "abc123",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	public := cfg.Public()
	if !public.ProtectKeySet {
		t.Error("the view should report that a key is configured")
	}
	for _, secret := range []string{"super-secret-key", "password"} {
		if strings.Contains(public.CameraTarget, secret) {
			t.Errorf("CameraTarget leaked %q: %s", secret, public.CameraTarget)
		}
	}
}

func TestPublicActiveWindowFormatting(t *testing.T) {
	setEnv(t, map[string]string{
		"CAMERA_SNAPSHOT_URL": "http://camera.example.lan/snap.jpeg",
		"ACTIVE_HOURS":        "05-21",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Public().ActiveWindow; got != "05:00 – 21:59" {
		t.Errorf("ActiveWindow = %q", got)
	}
}
