package web

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Frontend failures are otherwise invisible to whoever runs this: they have the
// service log and nothing else, and a browser console they never open is not a
// diagnostic. This endpoint exists so a JavaScript fault shows up in the same
// place as everything else.
//
// It is an unauthenticated write in the default configuration, so it is kept
// deliberately small: a capped body, a rate limit, and log output only.
const (
	clientErrorMaxBody  = 4 << 10
	clientErrorMinDelay = 2 * time.Second
	clientErrorMaxField = 512
)

type clientErrorLimiter struct {
	mu   sync.Mutex
	last time.Time
}

func (l *clientErrorLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if time.Since(l.last) < clientErrorMinDelay {
		return false
	}
	l.last = time.Now()
	return true
}

func (s *Server) handleClientError(w http.ResponseWriter, r *http.Request) {
	// Accepted regardless, so the browser never retries or surfaces a failure
	// for what is only a diagnostic.
	defer w.WriteHeader(http.StatusNoContent)

	if !s.clientErrors.allow() {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, clientErrorMaxBody))
	if err != nil {
		return
	}

	var payload struct {
		Context string `json:"context"`
		Detail  string `json:"detail"`
		Page    string `json:"page"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}

	s.log.Warn("browser reported an error",
		"context", truncate(payload.Context, clientErrorMaxField),
		"detail", truncate(payload.Detail, clientErrorMaxField),
		"page", truncate(payload.Page, clientErrorMaxField),
		"userAgent", truncate(r.UserAgent(), clientErrorMaxField))
}

// truncate bounds a field from an untrusted source before it reaches the log.
func truncate(v string, limit int) string {
	if len(v) <= limit {
		return v
	}
	return v[:limit] + "…"
}
