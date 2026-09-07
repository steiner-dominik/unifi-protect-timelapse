package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFrameAt(t *testing.T, root, day, prefix, clock string) {
	t.Helper()
	dir := filepath.Join(root, day[:4], day[:7], day)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	name := prefix + day + "-" + clock + ".jpg"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("jpeg"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestReportFindsNoGapsInAContinuousDay(t *testing.T) {
	b, cfg := newTestBrowser(t)
	for _, clock := range []string{"05-00-00", "05-05-00", "05-10-00", "05-15-00"} {
		writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_", clock)
	}

	report, err := b.Report("2026-09-07", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if report.Frames != 4 {
		t.Errorf("Frames = %d, want 4", report.Frames)
	}
	if len(report.Gaps) != 0 {
		t.Errorf("expected no gaps, got %+v", report.Gaps)
	}
	if report.First != "05:00:00" || report.Last != "05:15:00" {
		t.Errorf("First/Last = %s/%s", report.First, report.Last)
	}
}

// This is the shape of the outage that went unnoticed: images stop for hours
// while everything else looks fine.
func TestReportDetectsAGap(t *testing.T) {
	b, cfg := newTestBrowser(t)
	for _, clock := range []string{"05-00-00", "05-05-00", "08-05-00", "08-10-00"} {
		writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_", clock)
	}

	report, err := b.Report("2026-09-07", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(report.Gaps) != 1 {
		t.Fatalf("expected exactly 1 gap, got %+v", report.Gaps)
	}

	gap := report.Gaps[0]
	if gap.After != "05:05:00" || gap.Before != "08:05:00" {
		t.Errorf("gap boundaries = %s -> %s", gap.After, gap.Before)
	}
	if gap.Seconds != 3*3600 {
		t.Errorf("gap length = %ds, want %d", gap.Seconds, 3*3600)
	}
	// Three hours at five minutes is 36 slots, one of which is the normal
	// spacing between the two frames that bound the gap.
	if gap.MissingFrames != 35 {
		t.Errorf("MissingFrames = %d, want 35", gap.MissingFrames)
	}
	if report.MissingFrames != 35 {
		t.Errorf("total MissingFrames = %d, want 35", report.MissingFrames)
	}
}

// A single late frame is normal jitter and must not be reported as a gap.
func TestReportToleratesOneLateFrame(t *testing.T) {
	b, cfg := newTestBrowser(t)
	for _, clock := range []string{"05-00-00", "05-09-00", "05-14-00"} {
		writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "snapshot_", clock)
	}

	report, err := b.Report("2026-09-07", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(report.Gaps) != 0 {
		t.Errorf("a 9 minute spacing at a 5 minute interval should be tolerated, got %+v", report.Gaps)
	}
}

// Two capture series in one day must be judged separately, or the interleaving
// of their timestamps would hide real gaps in each.
func TestReportFiltersByGroup(t *testing.T) {
	b, cfg := newTestBrowser(t)
	writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "steiner_attic_", "05-00-00")
	writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "steiner_attic_", "05-05-00")
	writeFrameAt(t, cfg.Sync.ArchiveDir, "2026-09-07", "steiner_attic_ui_", "05-00-00")

	all, err := b.Report("2026-09-07", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if all.Frames != 3 {
		t.Errorf("unfiltered Frames = %d, want 3", all.Frames)
	}

	filtered, err := b.Report("2026-09-07", "steiner_attic_ui_", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if filtered.Frames != 1 {
		t.Errorf("filtered Frames = %d, want 1", filtered.Frames)
	}
}

func TestReportOnAnEmptyDay(t *testing.T) {
	b, _ := newTestBrowser(t)
	report, err := b.Report("2026-09-07", "", 5*time.Minute)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if report.Frames != 0 || len(report.Gaps) != 0 {
		t.Errorf("unexpected report for an empty day: %+v", report)
	}
}

func TestReportRejectsAnInvalidDay(t *testing.T) {
	b, _ := newTestBrowser(t)
	if _, err := b.Report("../../etc", "", 5*time.Minute); err == nil {
		t.Fatal("expected an invalid day to be rejected")
	}
}
