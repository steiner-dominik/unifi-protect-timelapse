package schedule

import (
	"testing"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

func fixedConfig(t *testing.T, start, end int, interval time.Duration) *config.Config {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		t.Fatalf("loading location: %v", err)
	}
	return &config.Config{
		Location: loc,
		Capture:  config.Capture{Interval: interval},
		Schedule: config.Schedule{Mode: config.ScheduleFixed, StartHour: start, EndHour: end},
	}
}

// The Pi's cron expression was "*/5 5-21", which fires throughout the 21st
// hour. The replacement must keep that inclusive end.
func TestActiveMatchesCronSemantics(t *testing.T) {
	s := New(fixedConfig(t, 5, 21, 5*time.Minute))
	loc := s.cfg.Location

	cases := []struct {
		hour, minute int
		want         bool
	}{
		{4, 59, false},
		{5, 0, true},
		{13, 30, true},
		{21, 0, true},
		{21, 55, true},
		{22, 0, false},
		{23, 30, false},
	}

	for _, tc := range cases {
		at := time.Date(2026, 9, 7, tc.hour, tc.minute, 0, 0, loc)
		if got := s.Active(at); got != tc.want {
			t.Errorf("Active(%02d:%02d) = %v, want %v", tc.hour, tc.minute, got, tc.want)
		}
	}
}

func TestNextTickAlignsToWallClock(t *testing.T) {
	s := New(fixedConfig(t, 0, 23, 5*time.Minute))
	loc := s.cfg.Location

	at := time.Date(2026, 9, 7, 13, 32, 17, 0, loc)
	next := s.NextTick(at)

	if next.Minute() != 35 || next.Second() != 0 {
		t.Fatalf("expected the next tick at 13:35:00, got %s", next.Format("15:04:05"))
	}
}

// Outside the window the scheduler must jump to the next window start rather
// than stepping through the night interval by interval.
func TestNextCaptureSkipsToWindowStart(t *testing.T) {
	s := New(fixedConfig(t, 5, 21, 5*time.Minute))
	loc := s.cfg.Location

	at := time.Date(2026, 9, 7, 23, 10, 0, 0, loc)
	next, ok := s.NextCapture(at)
	if !ok {
		t.Fatal("expected a next capture time")
	}
	if next.Day() != 8 || next.Hour() != 5 || next.Minute() != 0 {
		t.Fatalf("expected 2026-09-08 05:00, got %s", next.Format(time.RFC3339))
	}
}

// A daylight-saving transition must not duplicate or skip a slot.
func TestNextTickAcrossDSTTransition(t *testing.T) {
	s := New(fixedConfig(t, 0, 23, 5*time.Minute))
	loc := s.cfg.Location

	// Central European Time moves forward at 02:00 on the last Sunday of March.
	at := time.Date(2026, 3, 29, 1, 57, 0, 0, loc)
	seen := make(map[int64]bool)

	for range 5 {
		at = s.NextTick(at)
		unix := at.Unix()
		if seen[unix] {
			t.Fatalf("duplicate tick at %s", at.Format(time.RFC3339))
		}
		seen[unix] = true
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct ticks, got %d", len(seen))
	}
}

func TestNextDailyAt(t *testing.T) {
	s := New(fixedConfig(t, 5, 21, 5*time.Minute))
	loc := s.cfg.Location

	// Before the target time on the same day.
	at := time.Date(2026, 9, 7, 20, 0, 0, 0, loc)
	if next := s.NextDailyAt(at, 23, 0); next.Day() != 7 || next.Hour() != 23 {
		t.Fatalf("expected the same day at 23:00, got %s", next.Format(time.RFC3339))
	}

	// After the target time, it must roll to tomorrow.
	at = time.Date(2026, 9, 7, 23, 30, 0, 0, loc)
	if next := s.NextDailyAt(at, 23, 0); next.Day() != 8 {
		t.Fatalf("expected the next day, got %s", next.Format(time.RFC3339))
	}
}

func TestSolarWindowIsPlausible(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		t.Fatalf("loading location: %v", err)
	}
	cfg := &config.Config{
		Location: loc,
		Capture:  config.Capture{Interval: 5 * time.Minute},
		Schedule: config.Schedule{
			Mode: config.ScheduleSolar,
			// Vienna.
			Latitude:  48.2082,
			Longitude: 16.3738,
		},
	}
	s := New(cfg)

	// Around the summer solstice Vienna gets roughly 16 hours of daylight.
	window := s.WindowFor(time.Date(2026, 6, 21, 12, 0, 0, 0, loc))
	if window.Never || window.AllDay {
		t.Fatal("expected a normal sunrise/sunset window in Vienna in June")
	}
	daylight := window.End.Sub(window.Start)
	if daylight < 15*time.Hour || daylight > 17*time.Hour {
		t.Fatalf("implausible daylight length %s", daylight)
	}
	if window.Start.Hour() < 3 || window.Start.Hour() > 6 {
		t.Fatalf("implausible sunrise at %s", window.Start.Format("15:04"))
	}

	// In December the day is much shorter.
	winter := s.WindowFor(time.Date(2026, 12, 21, 12, 0, 0, 0, loc))
	winterDaylight := winter.End.Sub(winter.Start)
	if winterDaylight > 9*time.Hour {
		t.Fatalf("December daylight should be short, got %s", winterDaylight)
	}
}

func TestSolarPolarDayAndNight(t *testing.T) {
	loc := time.UTC
	// Longyearbyen, Svalbard.
	cfg := &config.Config{
		Location: loc,
		Capture:  config.Capture{Interval: 5 * time.Minute},
		Schedule: config.Schedule{Mode: config.ScheduleSolar, Latitude: 78.22, Longitude: 15.65},
	}
	s := New(cfg)

	if summer := s.WindowFor(time.Date(2026, 6, 21, 12, 0, 0, 0, loc)); !summer.AllDay {
		t.Error("expected polar day in June at 78 degrees north")
	}
	if winter := s.WindowFor(time.Date(2026, 12, 21, 12, 0, 0, 0, loc)); !winter.Never {
		t.Error("expected polar night in December at 78 degrees north")
	}
}
