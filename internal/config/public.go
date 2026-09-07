package config

import (
	"net/url"
	"strings"
)

// Public is a view of the configuration that is safe to expose over HTTP. It
// deliberately carries no secret material: the Protect API key is reduced to a
// boolean, and URLs are stripped of any embedded credentials and query strings
// so that a token passed as a query parameter can never leak into the debug
// page.
type Public struct {
	Timezone        string `json:"timezone"`
	LogLevel        string `json:"logLevel"`
	CameraSource    string `json:"cameraSource"`
	CameraFallback  string `json:"cameraFallback"`
	CameraTarget    string `json:"cameraTarget"`
	ProtectKeySet   bool   `json:"protectKeySet"`
	ProtectInsecure bool   `json:"protectInsecureTls"`
	CaptureEnabled  bool   `json:"captureEnabled"`
	CaptureInterval string `json:"captureInterval"`
	FilenamePrefix  string `json:"filenamePrefix"`
	ScheduleMode    string `json:"scheduleMode"`
	ActiveWindow    string `json:"activeWindow"`
	SpoolDir        string `json:"spoolDir"`
	ArchiveDir      string `json:"archiveDir"`
	ArchiveSentinel string `json:"archiveSentinel"`
	SyncMode        string `json:"syncMode"`
	SyncAt          string `json:"syncAt"`
	SyncInterval    string `json:"syncInterval"`
	AuthEnabled     bool   `json:"authEnabled"`
	ArchiveBrowsing bool   `json:"archiveBrowsing"`
	LivePreview     bool   `json:"livePreview"`
	NotifyEnabled   bool   `json:"notifyEnabled"`
	MetricsEnabled  bool   `json:"metricsEnabled"`
	DefaultLanguage string `json:"defaultLanguage"`
	SiteName        string `json:"siteName"`
	MonitorEnabled  bool   `json:"monitorEnabled"`
	MonitorInterval string `json:"monitorInterval"`
	ArchiveMaxAge   string `json:"archiveMaxAge"`
	FrozenThreshold int    `json:"frozenThreshold"`
	AuthMode        string `json:"authMode"`
	VideoEnabled    bool   `json:"videoEnabled"`
	ZipEnabled      bool   `json:"zipEnabled"`
	ArchiveProxy    string `json:"archiveProxy"`
	HAPublish       bool   `json:"haPublish"`
}

// Public returns the redacted configuration view.
func (c *Config) Public() Public {
	p := Public{
		Timezone:        c.Location.String(),
		LogLevel:        c.LogLevel,
		CameraSource:    string(c.Camera.Kind),
		CameraFallback:  string(c.Camera.Fallback),
		ProtectKeySet:   c.Camera.SecretSet(),
		ProtectInsecure: c.Camera.InsecureTLS,
		CaptureEnabled:  c.Capture.Enabled,
		CaptureInterval: c.Capture.Interval.String(),
		FilenamePrefix:  c.Capture.FilenamePrefix,
		ScheduleMode:    string(c.Schedule.Mode),
		SpoolDir:        c.Capture.SpoolDir,
		ArchiveSentinel: c.Sync.Sentinel,
		SyncMode:        string(c.Sync.Mode),
		SyncAt:          c.Sync.At,
		SyncInterval:    syncIntervalLabel(c.Sync.Interval),
		AuthEnabled:     c.Web.AuthToken != "",
		ArchiveBrowsing: c.Web.ArchiveEnabled,
		LivePreview:     c.Web.LivePreview,
		NotifyEnabled:   c.Notify.WebhookURL != "",
		MetricsEnabled:  c.MetricsEnabled,
		DefaultLanguage: c.Web.DefaultLanguage,
		SiteName:        c.Web.SiteName,
		MonitorEnabled:  c.Monitor.Enabled,
		MonitorInterval: c.Monitor.Interval.String(),
		ArchiveMaxAge:   c.Monitor.ArchiveMaxAge.String(),
		FrozenThreshold: c.Monitor.FrozenThreshold,
		AuthMode:        string(c.Web.AuthMode),
		VideoEnabled:    c.Video.Enabled,
		ZipEnabled:      c.Web.ZipEnabled,
		ArchiveProxy:    SanitizeURL(c.Web.ArchiveProxyURL),
		HAPublish:       c.HA.PublishEnabled,
	}

	p.CameraTarget = c.describeSource(c.Camera.Kind)
	if c.Camera.Fallback != "" {
		p.CameraTarget += "  →  fallback: " + c.describeSource(c.Camera.Fallback)
	}

	// The archive is reported whenever it is configured, not only when this
	// instance syncs to it: a watchdog deployment has sync switched off but
	// still reads the archive to judge how fresh it is.
	p.ArchiveDir = c.Sync.ArchiveDir

	switch c.Schedule.Mode {
	case ScheduleSolar:
		p.ActiveWindow = "sunrise" + offsetLabel(c.Schedule.DawnOffset) + " – sunset" + offsetLabel(c.Schedule.DuskOffset)
	default:
		p.ActiveWindow = pad2(c.Schedule.StartHour) + ":00 – " + pad2(c.Schedule.EndHour) + ":59"
	}
	return p
}

// syncIntervalLabel renders the periodic sweep cadence, or empty when the
// periodic sweep is off.
func syncIntervalLabel(d interface{ String() string }) string {
	if s := d.String(); s != "0s" {
		return s
	}
	return ""
}

func offsetLabel(d interface{ String() string }) string {
	s := d.String()
	if strings.HasPrefix(s, "-") {
		return " " + s
	}
	if s == "0s" {
		return ""
	}
	return " +" + s
}

func pad2(v int) string {
	if v < 10 {
		return "0" + string(rune('0'+v))
	}
	return string(rune('0'+v/10)) + string(rune('0'+v%10))
}

// describeSource renders a source's endpoint without any secret material.
func (c *Config) describeSource(kind SourceKind) string {
	switch kind {
	case SourceProtect:
		return SanitizeURL(c.Camera.ProtectHost) + " (camera " + c.Camera.ProtectCameraID + ")"
	default:
		return SanitizeURL(c.Camera.SnapshotURL)
	}
}

// SanitizeURL removes userinfo and the query string from a URL so that neither
// credentials nor tokens can be rendered in the debug view.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
