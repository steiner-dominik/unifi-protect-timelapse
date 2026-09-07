package web

import (
	"net/http"
	"strings"
	"time"
)

// requestLog records every HTTP request.
//
// Without it there is no way to tell a browser that asked and got an error from
// a browser that never asked at all — which is exactly the ambiguity that made
// a stale cached page look like a server fault. Successful requests are logged
// at debug so normal operation stays quiet; anything that failed is logged at
// warning, because a failure nobody can see is the recurring theme of this
// service's bugs.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(recorder, r)

		// Static assets and the poll loop would otherwise dominate the log.
		if recorder.status < 400 && isRoutine(r.URL.Path) {
			return
		}

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.written,
			"duration", time.Since(started).Truncate(time.Millisecond),
		}
		// Named so a stale page shows up as its own version rather than as a
		// mystery.
		if version := r.URL.Query().Get("v"); version != "" {
			attrs = append(attrs, "assetVersion", version)
		}

		if recorder.status >= 400 {
			s.log.Warn("request failed", attrs...)
			return
		}
		s.log.Debug("request", attrs...)
	})
}

// isRoutine reports paths that are polled or fetched constantly and would drown
// the log at debug level.
func isRoutine(path string) bool {
	switch path {
	case "/healthz", "/metrics", "/api/status":
		return true
	}
	return strings.HasPrefix(path, "/static/")
}

// statusRecorder captures what was actually sent.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}
