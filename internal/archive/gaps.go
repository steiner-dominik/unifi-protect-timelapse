package archive

import (
	"time"
)

// Gap is a stretch of a day with no images.
type Gap struct {
	// After is the timestamp of the last frame before the gap, Before the
	// first frame after it.
	After  string `json:"after"`
	Before string `json:"before"`
	// Seconds is how long the gap lasted.
	Seconds int `json:"seconds"`
	// MissingFrames is how many frames the configured interval implies were
	// lost. It is an estimate: the interval may have been different when those
	// images were originally captured.
	MissingFrames int `json:"missingFrames"`
}

// DayReport summarises the completeness of one day.
type DayReport struct {
	Day    string `json:"day"`
	Frames int    `json:"frames"`
	First  string `json:"first"`
	Last   string `json:"last"`
	Gaps   []Gap  `json:"gaps"`
	// MissingFrames is the total implied by all gaps.
	MissingFrames int `json:"missingFrames"`
	// Interval is the spacing the report was computed against.
	IntervalSeconds int `json:"intervalSeconds"`
}

// Report analyses a day's frames for missing stretches.
//
// Gaps are derived from the spacing between the frames that are actually there,
// rather than from an expected schedule, because the capture interval and the
// active window have changed over the years and a historical day should not be
// judged against today's settings. A gap is any spacing longer than twice the
// interval, which tolerates one late frame without reporting noise.
func (b *Browser) Report(day string, group string, interval time.Duration) (*DayReport, error) {
	frames, err := b.Frames(day)
	if err != nil {
		return nil, err
	}
	if group != "" {
		filtered := frames[:0:0]
		for _, frame := range frames {
			if frame.Group == group {
				filtered = append(filtered, frame)
			}
		}
		frames = filtered
	}

	report := &DayReport{
		Day:             day,
		Frames:          len(frames),
		Gaps:            []Gap{},
		IntervalSeconds: int(interval.Seconds()),
	}
	if len(frames) == 0 {
		return report, nil
	}

	report.First = frames[0].Time
	report.Last = frames[len(frames)-1].Time

	if interval <= 0 {
		return report, nil
	}
	threshold := 2 * interval

	for i := 1; i < len(frames); i++ {
		previous, ok := parseFrameTime(day, frames[i-1].Time)
		if !ok {
			continue
		}
		current, ok := parseFrameTime(day, frames[i].Time)
		if !ok {
			continue
		}

		span := current.Sub(previous)
		if span <= threshold {
			continue
		}

		// One interval of the span is the normal spacing; the rest is missing.
		missing := int(span/interval) - 1
		report.Gaps = append(report.Gaps, Gap{
			After:         frames[i-1].Time,
			Before:        frames[i].Time,
			Seconds:       int(span.Seconds()),
			MissingFrames: missing,
		})
		report.MissingFrames += missing
	}
	return report, nil
}

// parseFrameTime combines a day and a wall-clock time into an instant. The
// location does not matter because only differences are used.
func parseFrameTime(day, clock string) (time.Time, bool) {
	if day == "" || clock == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse("2006-01-02 15:04:05", day+" "+clock)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
