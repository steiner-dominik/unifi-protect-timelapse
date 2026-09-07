package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/archive"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/metrics"
)

// StatusResponse is the payload the debug view renders.
type StatusResponse struct {
	Version   string        `json:"version"`
	Now       time.Time     `json:"now"`
	Uptime    string        `json:"uptime"`
	Config    config.Public `json:"config"`
	Capture   CaptureStatus `json:"capture"`
	Sync      SyncStatus    `json:"sync"`
	Languages []string      `json:"languages"`
}

// CaptureStatus describes the capture side of the service.
type CaptureStatus struct {
	Active              bool       `json:"active"`
	WindowStart         string     `json:"windowStart"`
	WindowEnd           string     `json:"windowEnd"`
	NextCapture         *time.Time `json:"nextCapture"`
	LastAttempt         *time.Time `json:"lastAttempt"`
	LastSuccess         *time.Time `json:"lastSuccess"`
	LastError           string     `json:"lastError"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	TotalOK             uint64     `json:"totalOk"`
	TotalFailed         uint64     `json:"totalFailed"`
	LatestFilename      string     `json:"latestFilename"`
	LatestBytes         int64      `json:"latestBytes"`
	LatestCapturedAt    *time.Time `json:"latestCapturedAt"`
}

// SyncStatus describes the archive side of the service.
type SyncStatus struct {
	Enabled          bool       `json:"enabled"`
	ArchiveAvailable bool       `json:"archiveAvailable"`
	NextSync         *time.Time `json:"nextSync"`
	LastAttempt      *time.Time `json:"lastAttempt"`
	LastSuccess      *time.Time `json:"lastSuccess"`
	LastError        string     `json:"lastError"`
	LastFiles        int        `json:"lastFiles"`
	LastBytes        int64      `json:"lastBytes"`
	TotalOK          uint64     `json:"totalOk"`
	TotalFailed      uint64     `json:"totalFailed"`
	SpoolFiles       int        `json:"spoolFiles"`
	SpoolBytes       int64      `json:"spoolBytes"`
	SpoolOldest      *time.Time `json:"spoolOldest"`
}

var startedAt = time.Now()

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data := s.store.Get()
	now := time.Now().In(s.cfg.Location)
	window := s.scheduler.WindowFor(now)
	spool := s.syncer.Spool()

	resp := StatusResponse{
		Version:   s.version,
		Now:       now,
		Uptime:    time.Since(startedAt).Truncate(time.Second).String(),
		Config:    s.cfg.Public(),
		Languages: s.langs,
		Capture: CaptureStatus{
			Active:              s.cfg.Capture.Enabled && s.scheduler.Active(now),
			LastAttempt:         optionalTime(data.LastCaptureAttempt),
			LastSuccess:         optionalTime(data.LastCaptureSuccess),
			LastError:           data.LastCaptureError,
			ConsecutiveFailures: data.ConsecutiveFailures,
			TotalOK:             data.CapturesOK,
			TotalFailed:         data.CapturesFailed,
		},
		Sync: SyncStatus{
			Enabled:          s.cfg.SyncEnabled(),
			ArchiveAvailable: s.syncer.ArchiveAvailable(),
			LastAttempt:      optionalTime(data.LastSyncAttempt),
			LastSuccess:      optionalTime(data.LastSyncSuccess),
			LastError:        data.LastSyncError,
			LastFiles:        data.LastSyncFiles,
			LastBytes:        data.LastSyncBytes,
			TotalOK:          data.SyncRunsOK,
			TotalFailed:      data.SyncRunsFailed,
			SpoolFiles:       spool.Files,
			SpoolBytes:       spool.Bytes,
		},
	}

	switch {
	case window.Never:
		resp.Capture.WindowStart, resp.Capture.WindowEnd = "-", "-"
	case window.AllDay:
		resp.Capture.WindowStart, resp.Capture.WindowEnd = "00:00", "23:59"
	default:
		resp.Capture.WindowStart = window.Start.Format("15:04")
		resp.Capture.WindowEnd = window.End.Format("15:04")
	}

	if s.cfg.Capture.Enabled {
		if next, ok := s.scheduler.NextCapture(now); ok {
			resp.Capture.NextCapture = &next
		}
	}
	if data.Latest != nil {
		resp.Capture.LatestFilename = data.Latest.Filename
		resp.Capture.LatestBytes = data.Latest.Bytes
		resp.Capture.LatestCapturedAt = optionalTime(data.Latest.CapturedAt)
	}
	if s.cfg.SyncEnabled() {
		hour, minute := s.cfg.SyncClock()
		next := s.scheduler.NextDailyAt(now, hour, minute)
		resp.Sync.NextSync = &next
	}
	if spool.Oldest > 0 {
		oldest := time.Unix(spool.Oldest, 0).In(s.cfg.Location)
		resp.Sync.SpoolOldest = &oldest
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	healthy, reason := s.Healthy()
	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"reason":  reason,
		"version": s.version,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = metrics.Write(w, metrics.Input{
		Version:          s.version,
		State:            s.store.Get(),
		Spool:            s.syncer.Spool(),
		ArchiveAvailable: s.syncer.ArchiveAvailable(),
		SyncEnabled:      s.cfg.SyncEnabled(),
	})
}

// handleLatest serves the most recently captured frame from the state mirror,
// which stays available regardless of whether the original has been synced.
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	path := s.store.LatestImagePath()
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "no image captured yet", http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "cannot read image", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// Revalidate every time: the file is replaced in place on each capture.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "latest.jpg", info.ModTime(), file)
}

// handleLive fetches a frame straight from the camera for display only. It is
// never written to the archive, so the capture interval stays uniform. Results
// are briefly cached to protect the camera from a reloading browser.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Web.LivePreview {
		http.Error(w, "live preview is disabled", http.StatusForbidden)
		return
	}

	s.liveMu.Lock()
	if s.liveData != nil && time.Since(s.liveLast) < s.cfg.Web.LiveMinInterval {
		data, modified := s.liveData, s.liveTime
		s.liveMu.Unlock()
		serveImage(w, r, data, modified)
		return
	}
	s.liveMu.Unlock()

	ctx, cancel := contextWithTimeout(r, s.cfg.Camera.Timeout)
	defer cancel()

	data, err := s.capturer.Live(ctx)
	if err != nil {
		s.log.Warn("live preview failed", "error", err)
		http.Error(w, "camera did not return an image", http.StatusBadGateway)
		return
	}

	now := time.Now()
	s.liveMu.Lock()
	s.liveData, s.liveLast, s.liveTime = data, now, now
	s.liveMu.Unlock()

	serveImage(w, r, data, now)
}

func (s *Server) handleYears(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"years": orEmpty(s.browser.Years())})
}

func (s *Server) handleMonths(w http.ResponseWriter, r *http.Request) {
	months, err := s.browser.Months(r.PathValue("year"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"months": orEmpty(months)})
}

func (s *Server) handleDays(w http.ResponseWriter, r *http.Request) {
	days, err := s.browser.Days(r.PathValue("month"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": orEmpty(days)})
}

func (s *Server) handleFrames(w http.ResponseWriter, r *http.Request) {
	frames, err := s.browser.Frames(r.PathValue("day"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	if frames == nil {
		frames = []archive.Frame{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"frames": frames})
}

func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	filePath, err := s.browser.FramePath(r.PathValue("day"), r.PathValue("name"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "cannot read image", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	// Archived frames never change, so they can be cached hard. This is what
	// makes timelapse playback smooth on a second pass.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, path.Base(filePath), info.ModTime(), file)
}

func (s *Server) handleLanguages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"languages": s.langs,
		"default":   s.defaultLanguage(),
	})
}

func (s *Server) handleTranslation(w http.ResponseWriter, r *http.Request) {
	lang := strings.ToLower(r.PathValue("lang"))
	if !slices.Contains(s.langs, lang) {
		http.Error(w, "unknown language", http.StatusNotFound)
		return
	}
	raw, err := fs.ReadFile(s.i18n, lang+".json")
	if err != nil {
		http.Error(w, "unknown language", http.StatusNotFound)
		return
	}
	if !json.Valid(raw) {
		http.Error(w, "translation file is not valid JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

// defaultLanguage returns the configured default when it exists, otherwise the
// first available language.
func (s *Server) defaultLanguage() string {
	if slices.Contains(s.langs, s.cfg.Web.DefaultLanguage) {
		return s.cfg.Web.DefaultLanguage
	}
	if len(s.langs) > 0 {
		return s.langs[0]
	}
	return "en"
}

// Healthy reports whether captures are happening as configured. This is the
// signal the Docker health check uses, and it exists because the original
// setup failed silently for weeks when the camera's IP address changed.
func (s *Server) Healthy() (bool, string) {
	if !s.cfg.Capture.Enabled {
		return true, "capture disabled"
	}

	now := time.Now().In(s.cfg.Location)
	if !s.scheduler.Active(now) {
		return true, "outside the capture window"
	}

	data := s.store.Get()
	if data.LastCaptureSuccess.IsZero() {
		// Allow one grace interval after start before reporting unhealthy.
		if time.Since(startedAt) < 2*s.cfg.Capture.Interval {
			return true, "starting up"
		}
		return false, "no successful capture since start"
	}

	if age := time.Since(data.LastCaptureSuccess); age > 2*s.cfg.Capture.Interval {
		return false, fmt.Sprintf("last successful capture was %s ago", age.Truncate(time.Second))
	}
	return true, "ok"
}

func writeArchiveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, archive.ErrInvalidName):
		http.Error(w, "invalid request", http.StatusBadRequest)
	case errors.Is(err, archive.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func serveImage(w http.ResponseWriter, r *http.Request, data []byte, modified time.Time) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "live.jpg", modified, bytes.NewReader(data))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(payload)
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
