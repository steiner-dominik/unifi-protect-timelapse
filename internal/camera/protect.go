package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// protectSource fetches through the UniFi Protect integration API on a UniFi OS
// console, authenticating with an API key created under
// Settings -> Control Plane -> Integrations. Compared with the camera's own
// anonymous snapshot this can return a full-resolution frame.
type protectSource struct {
	client *http.Client
	cfg    config.Camera
}

// integrationBase is the path prefix the UniFi OS console proxies to Protect.
const integrationBase = "/proxy/protect/integration/v1"

func (p *protectSource) Snapshot(ctx context.Context) ([]byte, error) {
	endpoint := fmt.Sprintf("%s%s/cameras/%s/snapshot",
		p.cfg.ProtectHost, integrationBase, url.PathEscape(p.cfg.ProtectCameraID))
	if p.cfg.ProtectHighQuality {
		endpoint += "?highQuality=true"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", p.cfg.ProtectAPIKey)
	req.Header.Set("Accept", "image/jpeg,image/*")
	return fetch(ctx, p.client, req, p.cfg.MinImageBytes)
}

func (p *protectSource) Describe() string {
	return fmt.Sprintf("protect integration api: %s (camera %s)",
		config.SanitizeURL(p.cfg.ProtectHost), p.cfg.ProtectCameraID)
}

// CameraInfo is one entry of the Protect camera list.
type CameraInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	State string `json:"state"`
}

// ListCameras queries the Protect integration API for the available cameras.
// It backs the `timelapse cameras` helper command, which exists so that
// PROTECT_CAMERA_ID can be discovered without hand-crafting a curl call.
func ListCameras(ctx context.Context, client *http.Client, host, apiKey string) ([]CameraInfo, error) {
	endpoint := strings.TrimSuffix(host, "/") + integrationBase + "/cameras"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s from %s", resp.Status, endpoint)
	}

	var cameras []CameraInfo
	if err := json.Unmarshal(body, &cameras); err != nil {
		return nil, fmt.Errorf("decoding camera list: %w", err)
	}
	return cameras, nil
}
