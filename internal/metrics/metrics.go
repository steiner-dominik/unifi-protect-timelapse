// Package metrics renders Prometheus text-format metrics. Writing the
// exposition format by hand keeps the client library out of the dependency
// tree; there are only a handful of series and none of them need histograms.
package metrics

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
)

// Input is everything needed to render the metrics.
type Input struct {
	Version string
	State   state.Data
	Spool   syncer.SpoolStats
	// ArchiveAvailable is checked live rather than read from state, so the
	// metric reflects the mount right now.
	ArchiveAvailable bool
	SyncEnabled      bool
}

// Write renders the metrics to w.
func Write(w io.Writer, in Input) error {
	var b strings.Builder

	metric(&b, "timelapse_build_info", "gauge",
		"Build information; the value is always 1.",
		fmt.Sprintf("timelapse_build_info{version=%q} 1", in.Version))

	metric(&b, "timelapse_captures_total", "counter",
		"Total number of capture attempts by result.",
		"timelapse_captures_total{result=\"ok\"} "+strconv.FormatUint(in.State.CapturesOK, 10),
		"timelapse_captures_total{result=\"failed\"} "+strconv.FormatUint(in.State.CapturesFailed, 10))

	metric(&b, "timelapse_capture_consecutive_failures", "gauge",
		"Number of capture failures since the last success.",
		"timelapse_capture_consecutive_failures "+strconv.Itoa(in.State.ConsecutiveFailures))

	metric(&b, "timelapse_last_capture_success_timestamp_seconds", "gauge",
		"Unix timestamp of the last successful capture.",
		"timelapse_last_capture_success_timestamp_seconds "+unix(in.State.LastCaptureSuccess.Unix(), in.State.LastCaptureSuccess.IsZero()))

	metric(&b, "timelapse_last_capture_attempt_timestamp_seconds", "gauge",
		"Unix timestamp of the last capture attempt.",
		"timelapse_last_capture_attempt_timestamp_seconds "+unix(in.State.LastCaptureAttempt.Unix(), in.State.LastCaptureAttempt.IsZero()))

	metric(&b, "timelapse_sync_runs_total", "counter",
		"Total number of sync runs by result.",
		"timelapse_sync_runs_total{result=\"ok\"} "+strconv.FormatUint(in.State.SyncRunsOK, 10),
		"timelapse_sync_runs_total{result=\"failed\"} "+strconv.FormatUint(in.State.SyncRunsFailed, 10))

	metric(&b, "timelapse_last_sync_success_timestamp_seconds", "gauge",
		"Unix timestamp of the last successful sync.",
		"timelapse_last_sync_success_timestamp_seconds "+unix(in.State.LastSyncSuccess.Unix(), in.State.LastSyncSuccess.IsZero()))

	metric(&b, "timelapse_last_sync_files", "gauge",
		"Number of files moved by the last successful sync.",
		"timelapse_last_sync_files "+strconv.Itoa(in.State.LastSyncFiles))

	metric(&b, "timelapse_spool_files", "gauge",
		"Number of images currently buffered in the spool directory.",
		"timelapse_spool_files "+strconv.Itoa(in.Spool.Files))

	metric(&b, "timelapse_spool_bytes", "gauge",
		"Total size of images currently buffered in the spool directory.",
		"timelapse_spool_bytes "+strconv.FormatInt(in.Spool.Bytes, 10))

	metric(&b, "timelapse_spool_oldest_timestamp_seconds", "gauge",
		"Modification time of the oldest buffered image, 0 when the spool is empty.",
		"timelapse_spool_oldest_timestamp_seconds "+strconv.FormatInt(in.Spool.Oldest, 10))

	metric(&b, "timelapse_archive_available", "gauge",
		"1 when the archive sentinel file is present, 0 otherwise.",
		"timelapse_archive_available "+boolValue(in.ArchiveAvailable))

	metric(&b, "timelapse_sync_enabled", "gauge",
		"1 when images are moved out of the spool, 0 when SYNC_MODE=off.",
		"timelapse_sync_enabled "+boolValue(in.SyncEnabled))

	_, err := io.WriteString(w, b.String())
	return err
}

func metric(b *strings.Builder, name, kind, help string, samples ...string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	for _, sample := range samples {
		b.WriteString(sample)
		b.WriteByte('\n')
	}
}

// unix renders a timestamp, using 0 for "never happened" so that Prometheus
// queries can distinguish it from a real time.
func unix(v int64, zero bool) string {
	if zero {
		return "0"
	}
	return strconv.FormatInt(v, 10)
}

func boolValue(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
