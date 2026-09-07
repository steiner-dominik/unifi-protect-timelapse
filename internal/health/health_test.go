package health

import (
	"strings"
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

func watchdogConfig() *config.Config {
	return &config.Config{
		Location: time.UTC,
		Capture:  config.Capture{Enabled: false, Interval: 5 * time.Minute},
		Monitor:  config.Monitor{Enabled: true, Interval: 5 * time.Minute, ArchiveMaxAge: 26 * time.Hour},
	}
}

func capturingConfig() *config.Config {
	return &config.Config{
		Location: time.UTC,
		Capture:  config.Capture{Enabled: true, Interval: 5 * time.Minute},
		Monitor:  config.Monitor{Enabled: false, Interval: 5 * time.Minute, ArchiveMaxAge: 15 * time.Minute},
	}
}

// An archive that has never held an image is not a failure. Treating it as one
// restarted the container in a loop with no explanation.
func TestEmptyArchiveIsNotUnhealthy(t *testing.T) {
	cfg := watchdogConfig()
	data := state.Data{LastProbeAt: time.Now(), CameraOnline: true}

	if v := Evaluate(cfg, data, true, time.Hour); !v.Healthy {
		t.Errorf("an archive with no images should not be unhealthy: %q", v.Reason)
	}
}

// A camera probe that has not run yet says nothing either way.
func TestUnprobedCameraIsNotUnhealthy(t *testing.T) {
	cfg := watchdogConfig()

	if v := Evaluate(cfg, state.Data{}, true, time.Hour); !v.Healthy {
		t.Errorf("no probe yet should not be unhealthy: %q", v.Reason)
	}
}

func TestUnreachableCameraIsUnhealthyAndSaysWhy(t *testing.T) {
	cfg := watchdogConfig()
	data := state.Data{
		LastProbeAt:    time.Now(),
		CameraOnline:   false,
		LastProbeError: "connection refused",
	}

	v := Evaluate(cfg, data, true, time.Hour)
	if v.Healthy {
		t.Fatal("an unreachable camera should be unhealthy")
	}
	if !strings.Contains(v.Reason, "connection refused") {
		t.Errorf("the reason should carry the underlying error, got %q", v.Reason)
	}
}

// A nightly bulk transfer leaves the archive hours old all day, which must not
// read as a failure under the watchdog default.
func TestNightlySyncArchiveStaysHealthyByDefault(t *testing.T) {
	cfg := watchdogConfig()
	data := state.Data{
		LastProbeAt:     time.Now(),
		CameraOnline:    true,
		ArchiveNewestAt: time.Now().Add(-18 * time.Hour),
	}

	if v := Evaluate(cfg, data, true, time.Hour); !v.Healthy {
		t.Errorf("an archive written nightly should be healthy during the day: %q", v.Reason)
	}
}

func TestTrulyStaleArchiveIsUnhealthy(t *testing.T) {
	cfg := watchdogConfig()
	data := state.Data{
		LastProbeAt:     time.Now(),
		CameraOnline:    true,
		ArchiveNewestAt: time.Now().Add(-48 * time.Hour),
	}

	v := Evaluate(cfg, data, true, time.Hour)
	if v.Healthy {
		t.Fatal("an archive two days old should be unhealthy")
	}
	if !strings.Contains(v.Reason, "old") {
		t.Errorf("reason = %q", v.Reason)
	}
}

func TestOutsideTheWindowIsAlwaysHealthy(t *testing.T) {
	cfg := watchdogConfig()
	data := state.Data{LastProbeAt: time.Now(), CameraOnline: false}

	if v := Evaluate(cfg, data, false, time.Hour); !v.Healthy {
		t.Errorf("nothing is expected outside the window: %q", v.Reason)
	}
}

// The subcommand passes a zero uptime when it cannot tell, and must then judge
// on the facts rather than skipping the check entirely.
func TestStartupGraceOnlyAppliesWithAKnownUptime(t *testing.T) {
	cfg := capturingConfig()
	data := state.Data{} // no capture recorded

	if v := Evaluate(cfg, data, true, time.Second); !v.Healthy {
		t.Errorf("within the grace period this should be healthy: %q", v.Reason)
	}
	if v := Evaluate(cfg, data, true, time.Hour); v.Healthy {
		t.Error("after the grace period a missing capture should be unhealthy")
	}
	if v := Evaluate(cfg, data, true, 0); v.Healthy {
		t.Error("with an unknown uptime the facts should decide, not the grace")
	}
}

func TestCapturingDeploymentHealth(t *testing.T) {
	cfg := capturingConfig()

	fresh := state.Data{LastCaptureSuccess: time.Now()}
	if v := Evaluate(cfg, fresh, true, time.Hour); !v.Healthy {
		t.Errorf("a recent capture should be healthy: %q", v.Reason)
	}

	stalled := state.Data{LastCaptureSuccess: time.Now().Add(-time.Hour)}
	if v := Evaluate(cfg, stalled, true, time.Hour); v.Healthy {
		t.Error("a stalled capture should be unhealthy")
	}

	frozen := state.Data{LastCaptureSuccess: time.Now(), FrameFrozen: true, IdenticalCount: 5}
	v := Evaluate(cfg, frozen, true, time.Hour)
	if v.Healthy {
		t.Error("a frozen camera should be unhealthy")
	}
	if !strings.Contains(v.Reason, "identical") {
		t.Errorf("reason = %q", v.Reason)
	}
}

func TestNothingEnabledIsHealthy(t *testing.T) {
	cfg := &config.Config{
		Location: time.UTC,
		Capture:  config.Capture{Enabled: false},
		Monitor:  config.Monitor{Enabled: false},
	}
	if v := Evaluate(cfg, state.Data{}, true, time.Hour); !v.Healthy {
		t.Errorf("with nothing enabled there is nothing to fail: %q", v.Reason)
	}
}

func TestStartupGraceIgnoresDisabledLoops(t *testing.T) {
	cfg := watchdogConfig()
	cfg.Capture.Interval = time.Hour // must not count, capture is off

	if grace := StartupGrace(cfg); grace != 10*time.Minute {
		t.Errorf("StartupGrace = %s, want twice the monitor interval", grace)
	}
}
