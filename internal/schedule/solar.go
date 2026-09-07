package schedule

import (
	"math"
	"time"
)

// This is the NOAA sunrise/sunset algorithm, implemented with the standard
// library alone so that solar scheduling costs no dependency. Accuracy is
// around a minute, which is far more than a timelapse needs.

const (
	// zenith is the solar zenith angle for official sunrise/sunset, including
	// the standard refraction correction of 50 arc minutes.
	zenith = 90.833
	deg    = math.Pi / 180
	rad    = 180 / math.Pi
)

// SunriseSunset returns sunrise and sunset (in UTC) for the calendar date of t
// at the given coordinates. ok is false when the sun neither rises nor sets on
// that day, which happens inside the polar circles.
func SunriseSunset(t time.Time, latitude, longitude float64) (rise, set time.Time, ok bool) {
	year, month, day := t.Date()
	dayOfYear := float64(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).YearDay())

	riseHour, okRise := solarEvent(dayOfYear, latitude, longitude, true)
	setHour, okSet := solarEvent(dayOfYear, latitude, longitude, false)
	if !okRise || !okSet {
		return time.Time{}, time.Time{}, false
	}

	midnight := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return midnight.Add(hoursToDuration(riseHour)), midnight.Add(hoursToDuration(setHour)), true
}

// AlwaysUp reports whether the sun stays above the horizon for the whole day,
// which is what distinguishes polar day from polar night when SunriseSunset
// finds no rise or set. The sun is lowest at local midnight, where
// sin(altitude) = -cos(latitude + declination); comparing that against the
// refracted horizon gives the answer.
func AlwaysUp(t time.Time, latitude float64) bool {
	year, month, day := t.Date()
	dayOfYear := float64(time.Date(year, month, day, 0, 0, 0, 0, time.UTC).YearDay())
	declination := solarDeclination(dayOfYear)

	sinAltitudeAtMidnight := -math.Cos((latitude + declination) * deg)
	return sinAltitudeAtMidnight > math.Cos(zenith*deg)
}

// solarDeclination returns the sun's declination in degrees for a day of the
// year.
func solarDeclination(dayOfYear float64) float64 {
	meanAnomaly := 0.9856*(dayOfYear+0.5) - 3.289
	trueLongitude := normalizeDegrees(meanAnomaly +
		1.916*math.Sin(meanAnomaly*deg) +
		0.020*math.Sin(2*meanAnomaly*deg) +
		282.634)
	return math.Asin(0.39782*math.Sin(trueLongitude*deg)) * rad
}

// solarEvent computes the UTC hour of sunrise (rising) or sunset for a day of
// the year at the given coordinates.
func solarEvent(dayOfYear, latitude, longitude float64, rising bool) (float64, bool) {
	longitudeHour := longitude / 15

	var approx float64
	if rising {
		approx = dayOfYear + (6-longitudeHour)/24
	} else {
		approx = dayOfYear + (18-longitudeHour)/24
	}

	meanAnomaly := 0.9856*approx - 3.289

	trueLongitude := normalizeDegrees(meanAnomaly +
		1.916*math.Sin(meanAnomaly*deg) +
		0.020*math.Sin(2*meanAnomaly*deg) +
		282.634)

	rightAscension := normalizeDegrees(math.Atan(0.91764*math.Tan(trueLongitude*deg)) * rad)
	// Right ascension must sit in the same quadrant as the true longitude.
	rightAscension += (math.Floor(trueLongitude/90) * 90) - (math.Floor(rightAscension/90) * 90)
	rightAscensionHours := rightAscension / 15

	sinDec := 0.39782 * math.Sin(trueLongitude*deg)
	cosDec := math.Cos(math.Asin(sinDec))

	cosHourAngle := (math.Cos(zenith*deg) - sinDec*math.Sin(latitude*deg)) /
		(cosDec * math.Cos(latitude*deg))
	if cosHourAngle > 1 || cosHourAngle < -1 {
		// The sun does not reach the zenith angle on this day.
		return 0, false
	}

	var hourAngle float64
	if rising {
		hourAngle = 360 - math.Acos(cosHourAngle)*rad
	} else {
		hourAngle = math.Acos(cosHourAngle) * rad
	}
	hourAngle /= 15

	localMeanTime := hourAngle + rightAscensionHours - 0.06571*approx - 6.622
	return normalizeHours(localMeanTime - longitudeHour), true
}

func normalizeDegrees(v float64) float64 {
	v = math.Mod(v, 360)
	if v < 0 {
		v += 360
	}
	return v
}

func normalizeHours(v float64) float64 {
	v = math.Mod(v, 24)
	if v < 0 {
		v += 24
	}
	return v
}

func hoursToDuration(hours float64) time.Duration {
	return time.Duration(hours * float64(time.Hour))
}
