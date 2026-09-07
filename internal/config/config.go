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

// AuthMode selects how the web interface authenticates callers.
type AuthMode string

const (
	// AuthNone leaves the interface open, appropriate on a trusted LAN.
	AuthNone AuthMode = "none"
	// AuthLocal requires the shared token configured in WEB_AUTH_TOKEN.
	AuthLocal AuthMode = "local"
	// AuthIngress trusts Home Assistant, which authenticates every request
	// before it reaches the ingress path.
	AuthIngress AuthMode = "ingress"
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
	Monitor  Monitor
	Video    Video
	HA       HomeAssistant

	StateDir       string
	MetricsEnabled bool
}

// Camera describes where snapshots come from.
type Camera struct {
	Kind SourceKind
	// Fallback is tried when the primary source fails. Empty means there is
	// no fallback. Both sources are configured independently, so the same
	// camera can be reached through the Protect API and, if that is
	// unavailable, through its own anonymous snapshot endpoint.
	Fallback SourceKind

	// Anonymous snapshot mode.
	SnapshotURL string

	// UniFi Protect integration API mode.
	ProtectHost        string
	ProtectAPIKey      string
	ProtectCameraID    string
	ProtectHighQuality bool

	// InsecureTLS disables certificate verification for every camera request,
	// not only the Protect console. Cameras present self-signed certificates
	// too, and their snapshot endpoint is reached over HTTPS just as often.
	InsecureTLS bool

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
	Mode       SyncMode
	ArchiveDir string
	Sentinel   string
	At         string // "HH:MM" in the configured timezone.
	// Interval runs an additional sweep on a fixed cadence. It exists for the
	// split deployment, where capture happens in a different container and so
	// cannot trigger the opportunistic sync directly. Zero disables it.
	Interval       time.Duration
	PruneEmptyDirs bool
}

// Monitor describes the watchdog loop. It runs without capturing anything and
// exists for deployments that only observe: the Home Assistant add-on checks
// that the camera answers and that images keep landing in the archive, without
// writing a single file itself.
type Monitor struct {
	Enabled  bool
	Interval time.Duration
	// ArchiveMaxAge is how stale the newest archived image may become before
	// the service reports unhealthy. Zero means derive it from the capture
	// interval.
	ArchiveMaxAge time.Duration
	// FrozenThreshold is how many byte-identical frames in a row mean the
	// camera has stopped producing new images while still answering. Zero
	// disables the check.
	FrozenThreshold int
}

// Video describes on-demand timelapse rendering.
type Video struct {
	Enabled   bool
	FPS       int
	CRF       int
	MaxFrames int
	Timeout   time.Duration
	FFmpeg    string
}

// HomeAssistant describes pushing state into Home Assistant. When the service
// runs as an add-on the Supervisor injects a token, and entities can be written
// straight to the Core REST API without a broker or any client library.
type HomeAssistant struct {
	PublishEnabled bool
	BaseURL        string
	Token          string
	EntityPrefix   string
	Interval       time.Duration
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
	AuthMode        AuthMode
	ZipEnabled      bool
	// ArchiveProxyURL points at another instance that has the archive mounted.
	// It exists so the capturing container never needs the archive mount and
	// therefore always starts, even while the NAS is unreachable.
	ArchiveProxyURL string
	// ArchiveProxyToken authenticates against that instance when it runs with
	// WEB_AUTH_MODE=local.
	ArchiveProxyToken string
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
		Fallback:           SourceKind(strings.ToLower(envStr("CAMERA_FALLBACK_SOURCE", ""))),
		SnapshotURL:        envStr("CAMERA_SNAPSHOT_URL", ""),
		ProtectHost:        strings.TrimSuffix(envStr("PROTECT_HOST", ""), "/"),
		ProtectAPIKey:      envStr("PROTECT_API_KEY", ""),
		ProtectCameraID:    envStr("PROTECT_CAMERA_ID", ""),
		ProtectHighQuality: envBool("PROTECT_HIGH_QUALITY", true, fail),
		// PROTECT_INSECURE_TLS is the original name, kept because it is already
		// in people's configurations; it was only ever applied to the Protect
		// source, which was a bug rather than a feature.
		InsecureTLS:   envBool("CAMERA_INSECURE_TLS", envBool("PROTECT_INSECURE_TLS", false, fail), fail),
		Timeout:       envDur("CAMERA_TIMEOUT", 20*time.Second, fail),
		Retries:       envInt("CAMERA_RETRIES", 3, fail),
		RetryDelay:    envDur("CAMERA_RETRY_DELAY", 3*time.Second, fail),
		MinImageBytes: int64(envInt("MIN_IMAGE_BYTES", 1024, fail)),
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
		Interval:       envDur("SYNC_INTERVAL", 0, fail),
		PruneEmptyDirs: envBool("SYNC_PRUNE_EMPTY_DIRS", true, fail),
	}

	cfg.Web = Web{
		Enabled:           envBool("WEB_ENABLED", true, fail),
		Addr:              envStr("WEB_ADDR", ":8080"),
		AuthToken:         envStr("WEB_AUTH_TOKEN", ""),
		DefaultLanguage:   strings.ToLower(envStr("WEB_DEFAULT_LANGUAGE", "en")),
		I18nDir:           envStr("WEB_I18N_DIR", ""),
		SiteName:          envStr("WEB_SITE_NAME", ""),
		ArchiveEnabled:    envBool("WEB_ARCHIVE_ENABLED", true, fail),
		LivePreview:       envBool("WEB_LIVE_PREVIEW", true, fail),
		LiveMinInterval:   envDur("WEB_LIVE_MIN_INTERVAL", 2*time.Second, fail),
		AuthMode:          AuthMode(strings.ToLower(envStr("WEB_AUTH_MODE", ""))),
		ZipEnabled:        envBool("WEB_ZIP_ENABLED", true, fail),
		ArchiveProxyURL:   strings.TrimSuffix(envStr("ARCHIVE_PROXY_URL", ""), "/"),
		ArchiveProxyToken: envStr("ARCHIVE_PROXY_TOKEN", ""),
	}
	if cfg.Web.AuthMode == "" {
		// Without an explicit mode, a configured token means local auth and
		// no token means an open interface.
		cfg.Web.AuthMode = AuthNone
		if cfg.Web.AuthToken != "" {
			cfg.Web.AuthMode = AuthLocal
		}
	}

	cfg.Monitor = Monitor{
		// A deployment that does not capture is by definition only watching,
		// so the watchdog defaults on exactly there.
		Enabled:         envBool("MONITOR_ENABLED", !cfg.Capture.Enabled, fail),
		Interval:        envDur("MONITOR_INTERVAL", 5*time.Minute, fail),
		ArchiveMaxAge:   envDur("ARCHIVE_MAX_AGE", 0, fail),
		FrozenThreshold: envInt("FROZEN_FRAME_THRESHOLD", 3, fail),
	}
	if cfg.Monitor.ArchiveMaxAge <= 0 {
		if cfg.Capture.Enabled {
			// This instance writes the images, so the archive should grow at
			// roughly the capture interval. Three of them tolerates a missed
			// capture and its retries without flapping.
			cfg.Monitor.ArchiveMaxAge = 3 * cfg.Capture.Interval
		} else {
			// This instance only watches, and it has no way to know how often
			// the archive is actually written. A nightly bulk transfer is a
			// perfectly normal arrangement, and against a three interval limit
			// it would look broken all day. A day and a bit is the safe default;
			// tighten ARCHIVE_MAX_AGE when images really do land continuously.
			cfg.Monitor.ArchiveMaxAge = 26 * time.Hour
		}
	}

	cfg.Video = Video{
		Enabled:   envBool("VIDEO_ENABLED", true, fail),
		FPS:       envInt("VIDEO_FPS", 12, fail),
		CRF:       envInt("VIDEO_CRF", 23, fail),
		MaxFrames: envInt("VIDEO_MAX_FRAMES", 5000, fail),
		Timeout:   envDur("VIDEO_TIMEOUT", 10*time.Minute, fail),
		FFmpeg:    envStr("FFMPEG_PATH", "ffmpeg"),
	}

	cfg.HA = HomeAssistant{
		// SUPERVISOR_TOKEN is injected by the Home Assistant Supervisor, so its
		// presence is a reliable signal that publishing entities is possible.
		PublishEnabled: envBool("HA_PUBLISH_ENABLED", os.Getenv("SUPERVISOR_TOKEN") != "", fail),
		BaseURL:        strings.TrimSuffix(envStr("HA_BASE_URL", "http://supervisor/core"), "/"),
		Token:          envStr("SUPERVISOR_TOKEN", envStr("HA_TOKEN", "")),
		EntityPrefix:   envStr("HA_ENTITY_PREFIX", "timelapse"),
		Interval:       envDur("HA_PUBLISH_INTERVAL", time.Minute, fail),
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

	errs = append(errs, c.validateSource(c.Camera.Kind, "CAMERA_SOURCE")...)

	if c.Camera.Fallback != "" {
		if c.Camera.Fallback == c.Camera.Kind {
			fail("CAMERA_FALLBACK_SOURCE must differ from CAMERA_SOURCE")
		} else {
			errs = append(errs, c.validateSource(c.Camera.Fallback, "CAMERA_FALLBACK_SOURCE")...)
		}
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
		if c.Sync.Interval != 0 && c.Sync.Interval < 10*time.Second {
			fail("SYNC_INTERVAL: must be at least 10s, or 0 to disable")
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

	switch c.Web.AuthMode {
	case AuthNone, AuthIngress:
	case AuthLocal:
		if c.Web.AuthToken == "" {
			fail("WEB_AUTH_TOKEN is required when WEB_AUTH_MODE=%s", AuthLocal)
		}
	default:
		fail("WEB_AUTH_MODE: must be %q, %q or %q, got %q",
			AuthNone, AuthLocal, AuthIngress, c.Web.AuthMode)
	}

	if c.Web.ArchiveProxyURL != "" {
		if u, err := url.Parse(c.Web.ArchiveProxyURL); err != nil {
			fail("ARCHIVE_PROXY_URL: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("ARCHIVE_PROXY_URL: scheme must be http or https")
		}
	}

	if c.Monitor.Enabled && c.Monitor.Interval < time.Second {
		fail("MONITOR_INTERVAL: must be at least 1s")
	}
	if c.Monitor.FrozenThreshold < 0 {
		fail("FROZEN_FRAME_THRESHOLD: must not be negative")
	}
	if c.Video.Enabled {
		if c.Video.FPS < 1 || c.Video.FPS > 120 {
			fail("VIDEO_FPS: must be between 1 and 120")
		}
		if c.Video.CRF < 0 || c.Video.CRF > 51 {
			fail("VIDEO_CRF: must be between 0 and 51")
		}
	}
	if c.HA.PublishEnabled && c.HA.Token == "" {
		fail("HA_PUBLISH_ENABLED is set but no SUPERVISOR_TOKEN or HA_TOKEN is available")
	}

	if !c.Capture.Enabled && !c.Monitor.Enabled && !c.SyncEnabled() && !c.Web.Enabled {
		fail("nothing is enabled: set at least one of CAPTURE_ENABLED, MONITOR_ENABLED, SYNC_MODE or WEB_ENABLED")
	}
	return errs
}

// validateSource checks the settings a particular snapshot source needs. It is
// shared by the primary and the fallback so both are held to the same standard
// and both report against the variable the operator actually set.
func (c *Config) validateSource(kind SourceKind, field string) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch kind {
	case SourceSnapshot:
		if c.Camera.SnapshotURL == "" {
			fail("CAMERA_SNAPSHOT_URL is required when %s=%s", field, SourceSnapshot)
		} else if u, err := url.Parse(c.Camera.SnapshotURL); err != nil {
			fail("CAMERA_SNAPSHOT_URL: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("CAMERA_SNAPSHOT_URL: scheme must be http or https, got %q", u.Scheme)
		}
	case SourceProtect:
		if c.Camera.ProtectHost == "" {
			fail("PROTECT_HOST is required when %s=%s", field, SourceProtect)
		} else if u, err := url.Parse(c.Camera.ProtectHost); err != nil {
			fail("PROTECT_HOST: %w", err)
		} else if u.Scheme != "http" && u.Scheme != "https" {
			fail("PROTECT_HOST: must include scheme http:// or https://")
		}
		if c.Camera.ProtectAPIKey == "" {
			fail("PROTECT_API_KEY is required when %s=%s", field, SourceProtect)
		}
		if c.Camera.ProtectCameraID == "" {
			fail("PROTECT_CAMERA_ID is required when %s=%s (run `timelapse cameras` to list them)", field, SourceProtect)
		}
	default:
		fail("%s: must be %q or %q, got %q", field, SourceSnapshot, SourceProtect, kind)
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
