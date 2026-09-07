package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// upstreamCacheTTL bounds how often the status page reaches out to the archive
// service. The status endpoint is polled every 30 seconds by every open tab, so
// without this a handful of tabs would generate steady cross-container traffic.
const upstreamCacheTTL = 15 * time.Second

type upstreamCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	value     SyncStatus
	ok        bool
}

var upstreamState upstreamCache

// upstreamSync fetches the archive service's own view of the sync, so the main
// dashboard can show whether the NAS is mounted and when images last moved,
// even though this container has no archive mount at all.
func (s *Server) upstreamSync(ctx context.Context) (SyncStatus, bool) {
	upstreamState.mu.Lock()
	if time.Since(upstreamState.fetchedAt) < upstreamCacheTTL {
		value, ok := upstreamState.value, upstreamState.ok
		upstreamState.mu.Unlock()
		return value, ok
	}
	upstreamState.mu.Unlock()

	// A short deadline of its own: an unreachable archive service must slow the
	// status page down by a moment, not by the proxy client's full timeout.
	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	value, ok := s.fetchUpstreamSync(fetchCtx)

	upstreamState.mu.Lock()
	upstreamState.fetchedAt = time.Now()
	upstreamState.value = value
	upstreamState.ok = ok
	upstreamState.mu.Unlock()

	return value, ok
}

func (s *Server) fetchUpstreamSync(ctx context.Context) (SyncStatus, bool) {
	var empty SyncStatus

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.cfg.Web.ArchiveProxyURL+"/api/status", nil)
	if err != nil {
		return empty, false
	}
	req.Header.Set("Accept", "application/json")
	if token := s.cfg.Web.ArchiveProxyToken; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.proxy.Do(req)
	if err != nil {
		s.log.Debug("archive service status unavailable", "error", err)
		return empty, false
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return empty, false
	}

	var payload struct {
		Sync SyncStatus `json:"sync"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return empty, false
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return empty, false
	}
	return payload.Sync, true
}
