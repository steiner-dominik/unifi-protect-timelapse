package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/capture"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/hass"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/health"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/notify"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/schedule"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
)

// runner owns the two background loops.
type runner struct {
	cfg       *config.Config
	log       *slog.Logger
	store     *state.Store
	capturer  *capture.Capturer
	syncer    *syncer.Syncer
	scheduler *schedule.Scheduler
	notifier  *notify.Notifier
}

// captureLoop fires at each wall-clock aligned interval inside the active
// window. Ticks are recomputed after every capture rather than using a fixed
// ticker, so a slow capture cannot make the schedule drift.
func (r *runner) captureLoop(ctx context.Context) error {
	for {
		next, ok := r.scheduler.NextCapture(time.Now().In(r.cfg.Location))
		if !ok {
			r.log.Error("no capture time found within the next year; check SCHEDULE_MODE and ACTIVE_HOURS")
			return errors.New("capture schedule never becomes active")
		}

		wait := time.Until(next)
		r.log.Debug("waiting for next capture", "at", next.Format(time.RFC3339), "in", wait.Truncate(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		r.captureOnce(ctx)
	}
}

// healthWatch logs every change in the health verdict.
//
// The container health check runs as a separate process, so when it fails the
// reason goes to the container runtime rather than to this log. That made a
// restart loop look like the service was stopping for no reason. Reporting
// transitions here means the explanation is always in the log the operator
// actually reads.
func (r *runner) healthWatch(ctx context.Context) error {
	const interval = 30 * time.Second

	started := time.Now()
	evaluate := func() health.Verdict {
		active := r.scheduler.Active(time.Now().In(r.cfg.Location))
		return health.Evaluate(r.cfg, r.store.Get(), active, time.Since(started))
	}

	previous := evaluate()
	r.log.Info("health", "healthy", previous.Healthy, "reason", previous.Reason)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		current := evaluate()
		if current == previous {
			continue
		}

		if current.Healthy {
			r.log.Info("health recovered", "reason", current.Reason)
		} else {
			r.log.Error("unhealthy: the container runtime may restart this service",
				"reason", current.Reason)
			r.notifier.Send(ctx, notify.Event{
				Kind:     "unhealthy",
				Severity: "error",
				Title:    "Timelapse is unhealthy",
				Message:  current.Reason,
			})
		}
		previous = current
	}
}

// haSnapshot gathers the state Home Assistant entities are built from.
func (r *runner) haSnapshot(version string, sync *syncer.Syncer) hass.Input {
	in := hass.Input{
		Version:          version,
		Data:             r.store.Get(),
		Spool:            sync.Spool(),
		ArchiveAvailable: sync.ArchiveAvailable(),
		CaptureEnabled:   r.cfg.Capture.Enabled,
	}
	if r.cfg.Capture.Enabled {
		if next, ok := r.scheduler.NextCapture(time.Now().In(r.cfg.Location)); ok {
			in.NextCapture = &next
		}
	}
	return in
}

func (r *runner) captureOnce(ctx context.Context) {
	// Bound the whole attempt, retries included, so a hung camera can never
	// overlap the following interval.
	budget := min(r.cfg.Capture.Interval-time.Second,
		r.cfg.Camera.Timeout*time.Duration(r.cfg.Camera.Retries)+
			r.cfg.Camera.RetryDelay*time.Duration(r.cfg.Camera.Retries))
	if budget <= 0 {
		budget = r.cfg.Camera.Timeout
	}

	captureCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	result, err := r.capturer.Capture(captureCtx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		failures := r.store.Get().ConsecutiveFailures
		r.log.Error("capture failed", "error", err, "consecutiveFailures", failures)

		if failures >= r.cfg.Notify.FailureThreshold {
			r.notifier.Send(ctx, notify.Event{
				Kind:     "capture_failed",
				Severity: "error",
				Title:    "Timelapse capture is failing",
				Message: "The last " + strconv.Itoa(failures) + " capture attempts failed. Most recent error: " +
					err.Error(),
			})
		}
		return
	}

	r.notifier.Resolve("capture_failed")
	r.log.Info("captured", "file", result.Filename, "bytes", result.Bytes)

	// A camera that answers with the same bytes every time looks healthy by
	// every other measure, so it gets its own alert.
	if result.Frozen {
		r.log.Error("the camera is returning identical frames",
			"identicalFrames", result.IdenticalCount)
		r.notifier.Send(ctx, notify.Event{
			Kind:     "frame_frozen",
			Severity: "error",
			Title:    "Timelapse camera appears frozen",
			Message: "The last " + strconv.Itoa(result.IdenticalCount+1) +
				" frames were byte-identical. The camera is answering but no longer producing new images.",
		})
	} else if result.IdenticalCount == 0 {
		r.notifier.Resolve("frame_frozen")
	}

	// In opportunistic mode the frame is moved straight away when the archive
	// is reachable, so at most one interval of images sits on local disk.
	if r.cfg.Sync.Mode == config.SyncOpportunistic {
		r.syncNow(ctx, "opportunistic")
	}
}

// intervalSyncLoop runs a sweep on a fixed cadence. In the split deployment the
// capturing container cannot trigger an opportunistic sync here, so this is
// what keeps the window of images living only on local disk short.
func (r *runner) intervalSyncLoop(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.Sync.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.syncNow(ctx, "interval")
		}
	}
}

// syncLoop runs the nightly sweep, which is the catch-up path for everything
// buffered while the archive was unavailable.
func (r *runner) syncLoop(ctx context.Context) error {
	hour, minute := r.cfg.SyncClock()

	for {
		next := r.scheduler.NextDailyAt(time.Now().In(r.cfg.Location), hour, minute)
		r.log.Debug("waiting for nightly sync", "at", next.Format(time.RFC3339))

		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		r.syncNow(ctx, "nightly")
	}
}

// syncNow runs one sync pass and reports the outcome. An unavailable archive is
// logged at a lower level than a real failure: it is the expected, designed-for
// condition that the local spool exists to absorb.
func (r *runner) syncNow(ctx context.Context, trigger string) {
	report, err := r.syncer.Run(ctx)
	switch {
	case errors.Is(err, syncer.ErrArchiveUnavailable):
		spool := r.syncer.Spool()
		r.log.Warn("archive unavailable, keeping images in the local spool",
			"trigger", trigger,
			"sentinel", r.cfg.Sync.Sentinel,
			"archive", r.cfg.Sync.ArchiveDir,
			"bufferedFiles", spool.Files,
			"bufferedBytes", spool.Bytes)

		// Only worth notifying about on the nightly sweep; the opportunistic
		// path would otherwise report it every few minutes.
		if trigger == "nightly" {
			r.notifier.Send(ctx, notify.Event{
				Kind:     "archive_unavailable",
				Severity: "warning",
				Title:    "Timelapse archive is unavailable",
				Message: "The sentinel file " + r.cfg.Sync.Sentinel + " was not found in " +
					r.cfg.Sync.ArchiveDir + ". " + strconv.Itoa(spool.Files) +
					" images are buffered locally and will be transferred once the archive returns.",
			})
		}

	case err != nil:
		if ctx.Err() != nil {
			return
		}
		r.log.Error("sync failed", "trigger", trigger, "error", err)
		r.notifier.Send(ctx, notify.Event{
			Kind:     "sync_failed",
			Severity: "error",
			Title:    "Timelapse sync failed",
			Message:  err.Error(),
		})

	default:
		r.notifier.Resolve("archive_unavailable")
		r.notifier.Resolve("sync_failed")
		if report.Files > 0 {
			r.log.Info("moved images to the archive",
				"trigger", trigger,
				"files", report.Files,
				"bytes", report.Bytes,
				"duration", report.Duration.Truncate(time.Millisecond))
		} else {
			r.log.Debug("sync found nothing to move", "trigger", trigger)
		}
	}
}
