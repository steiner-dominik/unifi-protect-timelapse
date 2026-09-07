package web

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/archive"
)

// handleReport returns the completeness report for a day: how many frames are
// present and where the gaps are.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

	report, err := s.browser.Report(
		r.PathValue("day"),
		r.URL.Query().Get("group"),
		s.cfg.Capture.Interval,
	)
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleZip streams a day as a ZIP archive. Entries are stored rather than
// deflated: JPEGs are already compressed, so deflating them costs CPU and saves
// almost nothing.
func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}

	day := r.PathValue("day")
	frames, err := s.selectFrames(day, r.URL.Query().Get("group"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	if len(frames) == 0 {
		http.Error(w, "no frames for this day", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(day+".zip"))
	// The size is not known ahead of the stream, so the response cannot be
	// cached or ranged.
	w.Header().Set("Cache-Control", "no-store")

	writer := zip.NewWriter(w)
	defer func() { _ = writer.Close() }()

	for _, frame := range frames {
		path, err := s.browser.FramePath(day, frame.Name)
		if err != nil {
			continue
		}
		if err := addToZip(writer, path, frame.Name); err != nil {
			// The response is already streaming, so a status code can no longer
			// be sent; log it and stop cleanly with a truncated archive.
			s.log.Warn("writing zip entry failed", "frame", frame.Name, "error", err)
			return
		}
	}
}

func addToZip(writer *zip.Writer, path, name string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header := &zip.FileHeader{Name: name, Method: zip.Store}
	header.Modified = info.ModTime()

	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}

// handleVideo renders a day into an MP4 and streams it.
func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	if s.proxyArchive(w, r) {
		return
	}
	if s.video == nil || !s.video.Available() {
		http.Error(w, "video rendering is not available in this build", http.StatusNotImplemented)
		return
	}

	day := r.PathValue("day")
	frames, err := s.selectFrames(day, r.URL.Query().Get("group"))
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	if len(frames) == 0 {
		http.Error(w, "no frames for this day", http.StatusNotFound)
		return
	}

	paths := make([]string, 0, len(frames))
	for _, frame := range frames {
		path, err := s.browser.FramePath(day, frame.Name)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}

	fps := s.cfg.Video.FPS
	if raw := r.URL.Query().Get("fps"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 120 {
			fps = parsed
		}
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", contentDisposition(day+".mp4"))
	w.Header().Set("Cache-Control", "no-store")

	s.log.Info("rendering timelapse video", "day", day, "frames", len(paths), "fps", fps)

	if err := s.video.Render(r.Context(), paths, fps, w); err != nil {
		// Headers are already sent once rendering starts, so this can only be
		// logged.
		s.log.Error("rendering video failed", "day", day, "error", err)
	}
}

// selectFrames returns a day's frames, optionally narrowed to one capture
// series so a rendered video does not jump between camera framings.
func (s *Server) selectFrames(day, group string) ([]archive.Frame, error) {
	frames, err := s.browser.Frames(day)
	if err != nil {
		return nil, err
	}
	if group == "" {
		return frames, nil
	}

	filtered := make([]archive.Frame, 0, len(frames))
	for _, frame := range frames {
		if frame.Group == group {
			filtered = append(filtered, frame)
		}
	}
	return filtered, nil
}

// contentDisposition builds an attachment header. Names are generated from a
// validated date, so they need no escaping beyond quoting.
func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename=%q", name)
}

// proxyArchive forwards an archive request to another instance and reports
// whether it handled the request.
//
// This is what lets the capturing container run without the archive mount: it
// keeps starting and capturing while the NAS is unreachable, and asks the
// container that does hold the mount for anything archive related.
func (s *Server) proxyArchive(w http.ResponseWriter, r *http.Request) bool {
	upstream := s.cfg.Web.ArchiveProxyURL
	if upstream == "" {
		return false
	}

	target := upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	if _, err := url.Parse(target); err != nil {
		http.Error(w, "archive service is misconfigured", http.StatusInternalServerError)
		return true
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "archive service is unreachable", http.StatusBadGateway)
		return true
	}
	if token := s.cfg.Web.ArchiveProxyToken; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := s.proxy.Do(req)
	if err != nil {
		s.log.Warn("archive service is unreachable", "upstream", upstream, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "archive_unavailable",
			"detail": "the archive service did not respond",
		})
		return true
	}
	defer func() { _ = resp.Body.Close() }()

	for _, header := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Last-Modified"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.Header().Set("Cache-Control", resp.Header.Get("Cache-Control"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// latestFromArchive serves the newest archived frame. A deployment with capture
// disabled has no mirrored image of its own, so this is what the live view
// shows there.
func (s *Server) latestFromArchive(w http.ResponseWriter, r *http.Request) bool {
	path, modTime, err := s.newestArchiveFrame()
	if err != nil {
		return false
	}

	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(path), modTime, file)
	return true
}

// errNoArchiveFrame reports that nothing suitable was found.
var errNoArchiveFrame = errors.New("no archived frame")

func (s *Server) newestArchiveFrame() (string, time.Time, error) {
	if s.newestFrame == nil {
		return "", time.Time{}, errNoArchiveFrame
	}
	return s.newestFrame()
}
