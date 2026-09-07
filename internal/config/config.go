// Package config loads and validates all runtime configuration from the
// environment. Nothing in this project is configured any other way: there are
// no config files, no flags that carry secrets and no compiled-in defaults that
// reference a particular deployment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// SourceKind selects how a snapshot is obtained from the camera.
type SourceKind string

const (
	// SourceSnapshot fetches the camera's own anonymous snapshot endpoint.
	SourceSnapshot SourceKind = "snapshot"
	// SourceProtect fetches through the UniFi Protect integration API.
	SourceProtect SourceKind = "protect"
)

// ScheduleMode selects how the daily capture window is determined.
type ScheduleMode string

const (
	// ScheduleFixed uses a static hour range in the configured timezone.
	ScheduleFixed ScheduleMode = "fixed"
	// ScheduleSolar derives the window from sunrise and sunset.
	ScheduleSolar ScheduleMode = "solar"
)

// SyncMode selects when spooled images are moved to the archive.
type SyncMode string

const (
	// SyncOpportunistic moves each image right after capture and additionally
	// runs a nightly sweep to catch up on anything left behind.
	SyncOpportunistic SyncMode = "opportunistic"
	// SyncNightly only runs the nightly sweep.
	SyncNightly SyncMode = "nightly"
	// SyncOff never moves images; they stay in the spool directory.
	SyncOff SyncMode = "off"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Location *time.Location
	LogLevel string
	LogJSON  bool

	Camera   Camera
	Capture  Capture
	Schedule Schedule
	Sync     Sync
	Web      Web
	Notify   Notify

	StateDir       string
	MetricsEnabled bool
}

// Camera describes where snapshots come from.
type Camera struct {
	Kind SourceKind

	// Anonymous snapshot mode.
	SnapshotURL string

	// UniFi Protect integration API mode.
	ProtectHost        string
	ProtectAPIKey      string
	ProtectCameraID    string
	ProtectHighQuality bool
	ProtectInsecureTLS bool

	Timeout       time.Duration
	Retries       int
	RetryDelay    time.Duration
	MinImageBytes int64
}

// Capture describes how captured images are named and stored.
type Capture struct {
	Enabled        bool
	Interval       time.Duration
	FilenamePrefix string
	SpoolDir       string
}

// Schedule describes when the capture scheduler is active.
type Schedule struct {
	Mode      ScheduleMode
	StartHour int
	EndHour   int

	Latitude   float64
	Longitude  float64
	DawnOffset time.Duration
	DuskOffset time.Duration
}

// Sync describes how images are moved from the spool to the archive.
type Sync struct {
	Mode           SyncMode
	ArchiveDir     string
	Sentinel       string
	At             string // "HH:MM" in the configured timezone.
	PruneEmptyDirs bool
}

// Web describes the HTTP frontend.
type Web struct {
	Enabled         bool
	Addr            string
	AuthToken       string
	DefaultLanguage string
	I18nDir         string
	SiteName        string
	ArchiveEnabled  bool
	LivePreview     bool
	LiveMinInterval time.Duration
}

// Notify describes outbound failure notifications.
type Notify struct {
	WebhookURL       string
	FailureThreshold int
	Timeout          time.Duration
}

// SecretSet reports whether the Protect API key is configured, without
// revealing it.
func (c Camera) SecretSet() bool { return c.ProtectAPIKey != "" }

// Load reads the environment and returns a validated configuration.
func Load() (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	loc, err := time.LoadLocation(envStr("TZ", "UTC"))
	if err != nil {
		fail("TZ: %w", err)
		loc = time.UTC
	}

	cfg := &Config{
		Location:       loc,
		LogLevel:       strings.ToLower(envStr("LOG_LEVEL", "info")),
		LogJSON:        strings.EqualFold(envStr("LOG_FORMAT", "text"), "json"),
		StateDir:       envStr("STATE_DIR", "/state"),
		MetricsEnabled: envBool("METRICS_ENABLED", true, fail),
	}

	cfg.Camera = Camera{
		Kind:               SourceKind(strings.ToLower(envStr("CAMERA_SOURCE", string(SourceSnapshot)))),
		SnapshotURL:        envStr("CAMERA_SNAPSHOT_URL", ""),
		ProtectHost:        strings.TrimSuffix(envStr("PROTECT_HOST", ""), "/"),
		ProtectAPIKey:      envStr("PROTECT_API_KEY", ""),
		ProtectCameraID:    envStr("PROTECT_CAMERA_ID", ""),
		ProtectHighQuality: envBool("PROTECT_HIGH_QUALITY", true, fail),
		ProtectInsecureTLS: envBool("PROTECT_INSECURE_TLS", false, fail),
		Timeout:            envDur("CAMERA_TIMEOUT", 20*time.Second, fail),
		Retries:            envInt("CAMERA_RETRIES", 3, fail),
		RetryDelay:         envDur("CAMERA_RETRY_DELAY", 3*time.Second, fail),
		MinImageBytes:      int64(envInt("MIN_IMAGE_BYTES", 1024, fail)),
	}

	cfg.Capture = Capture{
		Enabled:        envBool("CAPTURE_ENABLED", true, fail),
		Interval:       envDur("CAPTURE_INTERVAL", 5*time.Minute, fail),
		FilenamePrefix: envStr("FILENAME_PREFIX", "snapshot_"),
		SpoolDir:       envStr("SPOOL_DIR", "/spool"),
	}

	cfg.Schedule = Schedule{
		Mode:       ScheduleMode(strings.ToLower(envStr("SCHEDULE_MODE", string(ScheduleFixed)))),
		Latitude:   envFloat("LATITUDE", 0, fail),
		Longitude:  envFloat("LONGITUDE", 0, fail),
		DawnOffset: envDur("SOLAR_DAWN_OFFSET", -30*time.Minute, fail),
		DuskOffset: envDur("SOLAR_DUSK_OFFSET", 30*time.Minute, fail),
	}
	cfg.Schedule.StartHour, cfg.Schedule.EndHour, err = parseHours(envStr("ACTIVE_HOURS", "05-21"))
	if err != nil {
		fail("ACTIVE_HOURS: %w", err)
	}

	cfg.Sync = Sync{
		Mode:           SyncMode(strings.ToLower(envStr("SYNC_MODE", string(SyncOpportunistic)))),
		ArchiveDir:     envStr("ARCHIVE_DIR", "/archive"),
		Sentinel:       envStr("ARCHIVE_SENTINEL", ".nas"),
		At:             envStr("SYNC_AT", "23:00"),
		PruneEmptyDirs: envBool("SYNC_PRUNE_EMPTY_DIRS", true, fail),
	}

	cfg.Web = Web{
		Enabled:         envBool("WEB_ENABLED", true, fail),
		Addr:            envStr("WEB_ADDR", ":8080"),
		AuthToken:       envStr("WEB_AUTH_TOKEN", ""),
		DefaultLanguage: strings.ToLower(envStr("WEB_DEFAULT_LANGUAGE", "en")),
		I18nDir:         envStr("WEB_I18N_DIR", ""),
		SiteName:        envStr("WEB_SITE_NAME", ""),
		ArchiveEnabled:  envBool("WEB_ARCHIVE_ENABLED", true, fail),
		LivePreview:     envBool("WEB_LIVE_PREVIEW", true, fail),
		LiveMinInterval: envDur("WEB_LIVE_MIN_INTERVAL", 2*time.Second, fail),
	}

	cfg.Notify = Notify{
		WebhookURL:       envStr("NOTIFY_WEBHOOK_URL", ""),
		FailureThreshold: envInt("NOTIFY_FAILURE_THRESHOLD", 3, fail),
		Timeout:          envDur("NOTIFY_TIMEOUT", 10*time.Second, fail),
	}

	errs = append(errs, cfg.validate()...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func (c *Config) validate() []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Camera.Kind {
	case SourceSnapshot:
		if c.Camera.SnapshotURL == "" {
			fail("CAMERA_SNAPSHOT_URL is required when CAMERA_SOURCE=snapshot")
		} else if u, err := url.Parse(c.Camera.SnapshotURL); err != nil {
			fail("CAMERA_SNAPSHOT_URL: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("CAMERA_SNAPSHOT_URL: scheme must be http or https, got %q", u.Scheme)
		}
	case SourceProtect:
		if c.Camera.ProtectHost == "" {
			fail("PROTECT_HOST is required when CAMERA_SOURCE=protect")
		} else if u, err := url.Parse(c.Camera.ProtectHost); err != nil {
			fail("PROTECT_HOST: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("PROTECT_HOST: must include scheme http:// or https://")
		}
		if c.Camera.ProtectAPIKey == "" {
			fail("PROTECT_API_KEY is required when CAMERA_SOURCE=protect")
		}
		if c.Camera.ProtectCameraID == "" {
			fail("PROTECT_CAMERA_ID is required when CAMERA_SOURCE=protect (see README for how to list camera IDs)")
		}
	default:
		fail("CAMERA_SOURCE: must be %q or %q, got %q", SourceSnapshot, SourceProtect, c.Camera.Kind)
	}

	if c.Camera.Retries < 1 {
		fail("CAMERA_RETRIES: must be at least 1")
	}
	if c.Camera.Timeout <= 0 {
		fail("CAMERA_TIMEOUT: must be positive")
	}
	if c.Capture.Interval < time.Second {
		fail("CAPTURE_INTERVAL: must be at least 1s")
	}
	if c.Capture.SpoolDir == "" {
		fail("SPOOL_DIR must not be empty")
	}
	if strings.ContainsAny(c.Capture.FilenamePrefix, `/\`) {
		fail("FILENAME_PREFIX must not contain path separators")
	}

	switch c.Schedule.Mode {
	case ScheduleFixed:
	case ScheduleSolar:
		if c.Schedule.Latitude == 0 && c.Schedule.Longitude == 0 {
			fail("LATITUDE and LONGITUDE are required when SCHEDULE_MODE=solar")
		}
		if c.Schedule.Latitude < -90 || c.Schedule.Latitude > 90 {
			fail("LATITUDE: must be between -90 and 90")
		}
		if c.Schedule.Longitude < -180 || c.Schedule.Longitude > 180 {
			fail("LONGITUDE: must be between -180 and 180")
		}
	default:
		fail("SCHEDULE_MODE: must be %q or %q, got %q", ScheduleFixed, ScheduleSolar, c.Schedule.Mode)
	}

	switch c.Sync.Mode {
	case SyncOpportunistic, SyncNightly:
		if c.Sync.ArchiveDir == "" {
			fail("ARCHIVE_DIR is required unless SYNC_MODE=off")
		}
		if c.Sync.Sentinel == "" {
			fail("ARCHIVE_SENTINEL must not be empty; it is the guard that prevents writing into an unmounted mountpoint")
		}
		if strings.ContainsAny(c.Sync.Sentinel, `/\`) {
			fail("ARCHIVE_SENTINEL must be a plain filename")
		}
		if _, _, err := parseClock(c.Sync.At); err != nil {
			fail("SYNC_AT: %w", err)
		}
		if c.Sync.ArchiveDir == c.Capture.SpoolDir {
			fail("ARCHIVE_DIR and SPOOL_DIR must not be the same directory")
		}
	case SyncOff:
	default:
		fail("SYNC_MODE: must be %q, %q or %q, got %q", SyncOpportunistic, SyncNightly, SyncOff, c.Sync.Mode)
	}

	if c.Web.Enabled && c.Web.Addr == "" {
		fail("WEB_ADDR must not be empty when WEB_ENABLED=true")
	}
	if c.Notify.WebhookURL != "" {
		if u, err := url.Parse(c.Notify.WebhookURL); err != nil {
			fail("NOTIFY_WEBHOOK_URL: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("NOTIFY_WEBHOOK_URL: scheme must be http or https")
		}
	}
	if c.StateDir == "" {
		fail("STATE_DIR must not be empty")
	}
	return errs
}

// SyncEnabled reports whether images are moved out of the spool at all.
func (c *Config) SyncEnabled() bool { return c.Sync.Mode != SyncOff }

// parseHours parses an "HH-HH" active window. The end hour is inclusive, so
// "05-21" keeps capturing until 21:59, matching the cron expression it
// replaces.
func parseHours(s string) (start, end int, err error) {
	before, after, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return 0, 0, fmt.Errorf("expected format HH-HH, got %q", s)
	}
	if start, err = strconv.Atoi(strings.TrimSpace(before)); err != nil {
		return 0, 0, fmt.Errorf("invalid start hour: %w", err)
	}
	if end, err = strconv.Atoi(strings.TrimSpace(after)); err != nil {
		return 0, 0, fmt.Errorf("invalid end hour: %w", err)
	}
	if start < 0 || start > 23 || end < 0 || end > 23 {
		return 0, 0, fmt.Errorf("hours must be between 0 and 23, got %q", s)
	}
	return start, end, nil
}

// parseClock parses an "HH:MM" time of day.
func parseClock(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("expected format HH:MM, got %q", s)
	}
	return t.Hour(), t.Minute(), nil
}

// SyncClock returns the configured nightly sync time of day.
func (c *Config) SyncClock() (hour, minute int) {
	h, m, err := parseClock(c.Sync.At)
	if err != nil {
		return 23, 0
	}
	return h, m
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool, fail func(string, ...any)) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		fail("%s: %w", key, err)
		return def
	}
	return v
}

func envInt(key string, def int, fail func(string, ...any)) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		fail("%s: %w", key, err)
		return def
	}
	return v
}

func envFloat(key string, def float64, fail func(string, ...any)) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		fail("%s: %w", key, err)
		return def
	}
	return v
}

func envDur(key string, def time.Duration, fail func(string, ...any)) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		fail("%s: %w", key, err)
		return def
	}
	return v
}
