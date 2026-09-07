// Package health decides whether the service is doing its job.
//
// There is exactly one implementation because there are three consumers that
// must never disagree: the /healthz endpoint, the `timelapse healthcheck`
// subcommand behind the container HEALTHCHECK, and the Home Assistant watchdog.
// They previously had separate logic, and the stricter one silently restarted
// the container in a loop while the endpoint reported everything was fine.
package health

import (
	"fmt"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// Verdict is the outcome of an evaluation.
type Verdict struct {
	Healthy bool
	Reason  string
}

// Evaluate reports whether the service is healthy.
//
// uptime is how long the service has been running; a zero value means unknown,
// in which case no startup grace is applied.
func Evaluate(cfg *config.Config, data state.Data, active bool, uptime time.Duration) Verdict {
	if !cfg.Capture.Enabled && !cfg.Monitor.Enabled {
		return Verdict{true, "neither capture nor monitoring is enabled"}
	}
	// Nothing is expected to happen outside the window, so nothing can be wrong.
	if !active {
		return Verdict{true, "outside the capture window"}
	}
	if grace := StartupGrace(cfg); uptime > 0 && uptime < grace {
		return Verdict{true, "starting up"}
	}

	if cfg.Capture.Enabled {
		switch {
		case data.LastCaptureSuccess.IsZero():
			return Verdict{false, "no successful capture since start"}
		case time.Since(data.LastCaptureSuccess) > 2*cfg.Capture.Interval:
			return Verdict{false, fmt.Sprintf("last successful capture was %s ago",
				time.Since(data.LastCaptureSuccess).Truncate(time.Second))}
		case data.FrameFrozen:
			return Verdict{false, fmt.Sprintf(
				"the camera has returned %d identical frames in a row", data.IdenticalCount)}
		}
	}

	if cfg.Monitor.Enabled {
		// A probe that has not run yet says nothing either way. Reporting
		// unhealthy here would restart the container before its first probe.
		if !data.LastProbeAt.IsZero() && !data.CameraOnline {
			reason := "the camera is not reachable"
			if data.LastProbeError != "" {
				reason += ": " + data.LastProbeError
			}
			return Verdict{false, reason}
		}
		// An archive with no images at all is not a failure: it may simply be
		// empty, or newly mounted. Only an archive that was growing and then
		// stopped is a problem worth restarting for.
		if !data.ArchiveNewestAt.IsZero() {
			if age := time.Since(data.ArchiveNewestAt); age > cfg.Monitor.ArchiveMaxAge {
				return Verdict{false, fmt.Sprintf(
					"newest archived image is %s old, limit is %s",
					age.Truncate(time.Second), cfg.Monitor.ArchiveMaxAge)}
			}
		}
	}
	return Verdict{true, "ok"}
}

// StartupGrace is how long after start the service is given before it can
// report unhealthy. Only the loops that actually run count, so a watchdog
// deployment is not held back by a capture interval it never uses.
func StartupGrace(cfg *config.Config) time.Duration {
	var grace time.Duration
	if cfg.Capture.Enabled {
		grace = 2 * cfg.Capture.Interval
	}
	if cfg.Monitor.Enabled {
		grace = max(grace, 2*cfg.Monitor.Interval)
	}
	if grace <= 0 {
		grace = time.Minute
	}
	return grace
}
