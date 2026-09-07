package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/archive"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/health"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/metrics"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
)

// StatusResponse is the payload the debug view renders.
type StatusResponse struct {
	Version   string        `json:"version"`
	Now       time.Time     `json:"now"`
	Uptime    string        `json:"uptime"`
	Config    config.Public `json:"config"`
	Health    Health        `json:"health"`
	Capture   CaptureStatus `json:"capture"`
	Sync      SyncStatus    `json:"sync"`
	Monitor   MonitorStatus `json:"monitor"`
	Languages []string      `json:"languages"`
}

// Health is the service's own verdict, so the UI, the container health check
// and the Home Assistant watchdog all agree rather than each deriving their own
// answer from the raw fields.
type Health struct {
	Healthy bool   `json:"healthy"`
	Reason  string `json:"reason"`
}

// MonitorStatus describes the watchdog: whether the camera answers and how
// fresh the archive is. It is populated whether or not this instance captures,
// so one status page serves both deployments.
type MonitorStatus struct {
	Enabled         bool       `json:"enabled"`
	CameraOnline    bool       `json:"cameraOnline"`
	LastProbe       *time.Time `json:"lastProbe"`
	LastProbeOK     *time.Time `json:"lastProbeOk"`
	LastProbeError  string     `json:"lastProbeError"`
	SourceInUse     string     `json:"sourceInUse"`
	ArchiveNewest   string     `json:"archiveNewest"`
	ArchiveNewestAt *time.Time `json:"archiveNewestAt"`
	ArchiveState    string     `json:"archiveState"`
	ArchiveError    string     `json:"archiveError"`
	// NetworkConflict is set when the camera address falls inside one of this
	// container's own subnets, which makes it unreachable from in here while
	// answering from the host.
	NetworkConflict string `json:"networkConflict"`
	ArchiveStale    bool   `json:"archiveStale"`
	ArchiveMaxAge   string `json:"archiveMaxAge"`
	FrameFrozen     bool   `json:"frameFrozen"`
	IdenticalFrames int    `json:"identicalFrames"`
	VideoAvailable  bool   `json:"videoAvailable"`
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
	Enabled bool `json:"enabled"`
	// Delegated is true when this instance has no archive of its own and asks
	// another one, which is how the split deployment keeps capture independent
	// of the NAS being reachable.
	Delegated        bool       `json:"delegated"`
	ServiceReachable bool       `json:"serviceReachable"`
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

	healthy, reason := s.Healthy()

	resp := StatusResponse{
		Version:   s.version,
		Health:    Health{Healthy: healthy, Reason: reason},
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
	resp.Monitor = MonitorStatus{
		Enabled:         s.cfg.Monitor.Enabled,
		CameraOnline:    data.CameraOnline,
		LastProbe:       optionalTime(data.LastProbeAt),
		LastProbeOK:     optionalTime(data.LastProbeOK),
		LastProbeError:  data.LastProbeError,
		SourceInUse:     data.CameraLastUsed,
		ArchiveNewest:   data.ArchiveNewest,
		ArchiveNewestAt: optionalTime(data.ArchiveNewestAt),
		ArchiveState:    data.ArchiveState,
		ArchiveError:    data.ArchiveError,
		NetworkConflict: s.networkConflict,
		ArchiveStale:    s.archiveStale(data),
		ArchiveMaxAge:   s.cfg.Monitor.ArchiveMaxAge.String(),
		FrameFrozen:     data.FrameFrozen,
		IdenticalFrames: data.IdenticalCount,
		VideoAvailable:  s.video != nil && s.video.Available(),
	}

	if spool.Oldest > 0 {
		oldest := time.Unix(spool.Oldest, 0).In(s.cfg.Location)
		resp.Sync.SpoolOldest = &oldest
	}

	// When the archive lives in another container, ask it rather than reporting
	// "sync disabled", which would be technically true and completely unhelpful.
	if s.cfg.Web.ArchiveProxyURL != "" {
		resp.Sync.Delegated = true
		if upstream, ok := s.upstreamSync(r.Context()); ok {
			resp.Sync.ServiceReachable = true
			resp.Sync.ArchiveAvailable = upstream.ArchiveAvailable
			resp.Sync.LastSuccess = upstream.LastSuccess
			resp.Sync.LastAttempt = upstream.LastAttempt
			resp.Sync.LastError = upstream.LastError
			resp.Sync.LastFiles = upstream.LastFiles
			resp.Sync.LastBytes = upstream.LastBytes
			resp.Sync.TotalOK = upstream.TotalOK
			resp.Sync.TotalFailed = upstream.TotalFailed
			resp.Sync.NextSync = upstream.NextSync
			resp.Sync.Enabled = upstream.Enabled
		}
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
		// A deployment that does not capture has no mirror of its own, so the
		// newest image in the archive stands in for it. This is what makes the
		// live view useful in a watchdog deployment.
		if s.latestFromArchive(w, r) {
			return
		}
		if data := s.store.Get(); data.ArchiveError != "" {
			s.log.Debug("no image to serve", "reason", data.ArchiveError)
		}
		http.Error(w, "no image available", http.StatusNotFound)
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
		http.Error(w, "camera did not return an image: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.log.Debug("live preview served", "bytes", len(data))

	now := time.Now()
	s.liveMu.Lock()
	s.liveData, s.liveLast, s.liveTime = data, now, now
	s.liveMu.Unlock()

	serveImage(w, r, data, now)
}

func (s *Server) handleYears(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"years": orEmpty(s.browser.Years())})
}

func (s *Server) handleMonths(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

	months, err := s.browser.Months(r.PathValue("year"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"months": orEmpty(months)})
}

func (s *Server) handleDays(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

	days, err := s.browser.Days(r.PathValue("month"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": orEmpty(days)})
}

func (s *Server) handleFrames(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

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
	if s.proxyArchive(w, r) {
		return
	}

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

// archiveStale reports whether the newest archived image has aged past the
// configured limit. It only means anything inside the capture window: outside
// it, nothing new is expected.
func (s *Server) archiveStale(data state.Data) bool {
	if data.ArchiveNewestAt.IsZero() {
		return false
	}
	if !s.scheduler.Active(time.Now().In(s.cfg.Location)) {
		return false
	}
	return time.Since(data.ArchiveNewestAt) > s.cfg.Monitor.ArchiveMaxAge
}

// Healthy reports whether the service is doing its job. The decision lives in
// the health package so this endpoint, the container health check and the Home
// Assistant watchdog cannot drift apart.
func (s *Server) Healthy() (bool, string) {
	now := time.Now().In(s.cfg.Location)
	verdict := health.Evaluate(s.cfg, s.store.Get(), s.scheduler.Active(now), time.Since(startedAt))
	return verdict.Healthy, verdict.Reason
}

// startupGrace is exposed for tests and mirrors the health package.
func (s *Server) startupGrace() time.Duration { return health.StartupGrace(s.cfg) }

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
