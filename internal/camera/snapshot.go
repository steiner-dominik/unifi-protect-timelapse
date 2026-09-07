package camera

import (
	"context"
	"net/http"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// snapshotSource fetches the camera's own anonymous snapshot endpoint, the
// method the original Raspberry Pi script used (curl against
// http://<camera-ip>/snap.jpeg). It requires "Anonymous Snapshot" to be enabled
// on the camera in UniFi Protect.
type snapshotSource struct {
	client *http.Client
	url    string
	cfg    config.Camera
}

func (s *snapshotSource) Snapshot(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/jpeg,image/*")
	return fetch(ctx, s.client, req, s.cfg.MinImageBytes)
}

func (s *snapshotSource) Describe() string {
	return "anonymous snapshot: " + config.SanitizeURL(s.url)
}
