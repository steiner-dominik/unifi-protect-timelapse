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
	AuthEnabled     bool   `json:"authEnabled"`
	ArchiveBrowsing bool   `json:"archiveBrowsing"`
	LivePreview     bool   `json:"livePreview"`
	NotifyEnabled   bool   `json:"notifyEnabled"`
	MetricsEnabled  bool   `json:"metricsEnabled"`
	DefaultLanguage string `json:"defaultLanguage"`
	SiteName        string `json:"siteName"`
}

// Public returns the redacted configuration view.
func (c *Config) Public() Public {
	p := Public{
		Timezone:        c.Location.String(),
		LogLevel:        c.LogLevel,
		CameraSource:    string(c.Camera.Kind),
		ProtectKeySet:   c.Camera.SecretSet(),
		ProtectInsecure: c.Camera.ProtectInsecureTLS,
		CaptureEnabled:  c.Capture.Enabled,
		CaptureInterval: c.Capture.Interval.String(),
		FilenamePrefix:  c.Capture.FilenamePrefix,
		ScheduleMode:    string(c.Schedule.Mode),
		SpoolDir:        c.Capture.SpoolDir,
		ArchiveSentinel: c.Sync.Sentinel,
		SyncMode:        string(c.Sync.Mode),
		SyncAt:          c.Sync.At,
		AuthEnabled:     c.Web.AuthToken != "",
		ArchiveBrowsing: c.Web.ArchiveEnabled,
		LivePreview:     c.Web.LivePreview,
		NotifyEnabled:   c.Notify.WebhookURL != "",
		MetricsEnabled:  c.MetricsEnabled,
		DefaultLanguage: c.Web.DefaultLanguage,
		SiteName:        c.Web.SiteName,
	}

	switch c.Camera.Kind {
	case SourceProtect:
		p.CameraTarget = SanitizeURL(c.Camera.ProtectHost) + " (camera " + c.Camera.ProtectCameraID + ")"
	default:
		p.CameraTarget = SanitizeURL(c.Camera.SnapshotURL)
	}

	if c.SyncEnabled() {
		p.ArchiveDir = c.Sync.ArchiveDir
	}

	switch c.Schedule.Mode {
	case ScheduleSolar:
		p.ActiveWindow = "sunrise" + offsetLabel(c.Schedule.DawnOffset) + " – sunset" + offsetLabel(c.Schedule.DuskOffset)
	default:
		p.ActiveWindow = pad2(c.Schedule.StartHour) + ":00 – " + pad2(c.Schedule.EndHour) + ":59"
	}
	return p
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
