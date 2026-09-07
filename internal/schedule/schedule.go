// Package schedule decides when the capture loop is allowed to run and when the
// next tick is due. Ticks are aligned to wall-clock boundaries so that a 5
// minute interval fires at :00, :05, :10 and so on, exactly as the cron
// expression it replaces did.
package schedule

import (
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// Window is a capture window on a particular day.
type Window struct {
	Start time.Time
	End   time.Time
	// AllDay is true when the window covers the whole day, either because it
	// was configured that way or because the sun does not rise or set at this
	// latitude on this date.
	AllDay bool
	// Never is true when capture is suppressed for the whole day (polar night
	// in solar mode).
	Never bool
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	switch {
	case w.Never:
		return false
	case w.AllDay:
		return true
	default:
		return !t.Before(w.Start) && !t.After(w.End)
	}
}

// Scheduler answers scheduling questions for a configuration.
type Scheduler struct {
	cfg *config.Config
}

// New returns a Scheduler for cfg.
func New(cfg *config.Config) *Scheduler { return &Scheduler{cfg: cfg} }

// WindowFor returns the capture window covering the day that t falls on, in the
// configured timezone.
func (s *Scheduler) WindowFor(t time.Time) Window {
	local := t.In(s.cfg.Location)
	year, month, day := local.Date()

	if s.cfg.Schedule.Mode == config.ScheduleSolar {
		rise, set, ok := SunriseSunset(local, s.cfg.Schedule.Latitude, s.cfg.Schedule.Longitude)
		if !ok {
			// Polar day or polar night. Above the horizon all day means capture
			// all day; below it means capture not at all.
			if AlwaysUp(local, s.cfg.Schedule.Latitude) {
				return Window{AllDay: true}
			}
			return Window{Never: true}
		}
		return Window{
			Start: rise.In(s.cfg.Location).Add(s.cfg.Schedule.DawnOffset),
			End:   set.In(s.cfg.Location).Add(s.cfg.Schedule.DuskOffset),
		}
	}

	start := s.cfg.Schedule.StartHour
	end := s.cfg.Schedule.EndHour
	if start == 0 && end == 23 {
		return Window{AllDay: true}
	}
	if start > end {
		// A window such as 22-04 wraps past midnight. Treat it as two spans by
		// reporting whichever one t is closest to; Contains handles the rest.
		return Window{
			Start:  time.Date(year, month, day, start, 0, 0, 0, s.cfg.Location),
			End:    time.Date(year, month, day+1, end, 59, 59, 0, s.cfg.Location),
			AllDay: false,
		}
	}
	return Window{
		Start: time.Date(year, month, day, start, 0, 0, 0, s.cfg.Location),
		// The end hour is inclusive, matching cron's "5-21" which fires
		// throughout the 21st hour.
		End: time.Date(year, month, day, end, 59, 59, 0, s.cfg.Location),
	}
}

// Active reports whether capture should happen at t.
func (s *Scheduler) Active(t time.Time) bool {
	local := t.In(s.cfg.Location)
	if s.WindowFor(local).Contains(local) {
		return true
	}
	// A window that wraps past midnight also covers the early hours of the
	// current day, which belong to yesterday's window.
	if s.cfg.Schedule.Mode == config.ScheduleFixed && s.cfg.Schedule.StartHour > s.cfg.Schedule.EndHour {
		return s.WindowFor(local.AddDate(0, 0, -1)).Contains(local)
	}
	return false
}

// NextTick returns the next wall-clock aligned instant at or after after.
// Alignment is computed in UTC so that a daylight-saving transition cannot
// produce a duplicated or skipped slot.
func (s *Scheduler) NextTick(after time.Time) time.Time {
	interval := s.cfg.Capture.Interval
	next := after.UTC().Truncate(interval)
	for !next.After(after) {
		next = next.Add(interval)
	}
	return next.In(s.cfg.Location)
}

// NextCapture returns the next instant at which a capture will actually happen,
// skipping ticks that fall outside the active window. It gives up after a year
// so that a pathological configuration cannot loop forever.
func (s *Scheduler) NextCapture(after time.Time) (time.Time, bool) {
	limit := after.AddDate(1, 0, 0)
	for t := s.NextTick(after); t.Before(limit); t = s.NextTick(t) {
		if s.Active(t) {
			return t, true
		}
		// Outside the window, jump straight to the start of the next window
		// instead of stepping interval by interval through the night.
		if next, ok := s.nextWindowStart(t); ok && next.After(t) {
			t = next.Add(-s.cfg.Capture.Interval)
		}
	}
	return time.Time{}, false
}

// nextWindowStart finds the start of the next window beginning after t.
func (s *Scheduler) nextWindowStart(t time.Time) (time.Time, bool) {
	local := t.In(s.cfg.Location)
	for day := range 366 {
		w := s.WindowFor(local.AddDate(0, 0, day))
		if w.Never {
			continue
		}
		if w.AllDay {
			return local, true
		}
		if w.Start.After(local) {
			return w.Start, true
		}
	}
	return time.Time{}, false
}

// NextDailyAt returns the next occurrence of hour:minute in the configured
// timezone strictly after t.
func (s *Scheduler) NextDailyAt(t time.Time, hour, minute int) time.Time {
	local := t.In(s.cfg.Location)
	year, month, day := local.Date()
	next := time.Date(year, month, day, hour, minute, 0, 0, s.cfg.Location)
	if !next.After(local) {
		next = time.Date(year, month, day+1, hour, minute, 0, 0, s.cfg.Location)
	}
	return next
}
