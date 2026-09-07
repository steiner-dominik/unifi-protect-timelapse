// Package hass publishes state into Home Assistant.
//
// When the service runs as a Home Assistant add-on the Supervisor injects a
// token, which is enough to write entity states straight to the Core REST API.
// That means real Home Assistant entities with no MQTT broker, no template
// sensors in YAML, and no client library: just HTTP against an endpoint that is
// already reachable from inside the add-on.
package hass

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/state"
	"github.com/steiner-dominik/unifi-protect-timelapse/internal/syncer"
)

// Publisher writes entity states to Home Assistant.
type Publisher struct {
	cfg    config.HomeAssistant
	client *http.Client
	log    *slog.Logger
}

// New returns a Publisher.
func New(cfg config.HomeAssistant, log *slog.Logger) *Publisher {
	return &Publisher{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
		log:    log,
	}
}

// Enabled reports whether publishing is configured.
func (p *Publisher) Enabled() bool {
	return p.cfg.PublishEnabled && p.cfg.Token != "" && p.cfg.BaseURL != ""
}

// Input is the state to publish.
type Input struct {
	Version          string
	Data             state.Data
	Spool            syncer.SpoolStats
	ArchiveAvailable bool
	CaptureEnabled   bool
	NextCapture      *time.Time
}

type entity struct {
	id         string
	state      string
	attributes map[string]any
}

// Run publishes on the configured interval until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context, snapshot func() Input) error {
	if !p.Enabled() {
		return nil
	}

	p.log.Info("publishing entities to Home Assistant",
		"prefix", p.cfg.EntityPrefix, "interval", p.cfg.Interval)

	// Publish immediately so the entities exist as soon as the add-on starts.
	p.Publish(ctx, snapshot())

	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.Publish(ctx, snapshot())
		}
	}
}

// Publish writes every entity once. Individual failures are logged and
// skipped: Home Assistant being briefly unavailable must never affect
// capturing.
func (p *Publisher) Publish(ctx context.Context, in Input) {
	if !p.Enabled() {
		return
	}
	for _, e := range p.entities(in) {
		if err := p.write(ctx, e); err != nil {
			p.log.Warn("publishing entity failed", "entity", e.id, "error", err)
		}
	}
}

func (p *Publisher) entities(in Input) []entity {
	prefix := p.cfg.EntityPrefix
	data := in.Data

	entities := []entity{
		{
			id:    "binary_sensor." + prefix + "_camera_online",
			state: onOff(data.CameraOnline),
			attributes: map[string]any{
				"friendly_name": "Timelapse camera online",
				"device_class":  "connectivity",
				"icon":          "mdi:cctv",
				"last_probe":    timestamp(data.LastProbeAt),
				"last_error":    data.LastProbeError,
				"source_in_use": data.CameraLastUsed,
			},
		},
		{
			id:    "binary_sensor." + prefix + "_archive_available",
			state: onOff(in.ArchiveAvailable),
			attributes: map[string]any{
				"friendly_name": "Timelapse archive available",
				"device_class":  "connectivity",
				"icon":          "mdi:nas",
			},
		},
		{
			id:    "binary_sensor." + prefix + "_frame_frozen",
			state: onOff(data.FrameFrozen),
			attributes: map[string]any{
				"friendly_name":    "Timelapse frame frozen",
				"device_class":     "problem",
				"icon":             "mdi:image-off",
				"identical_frames": data.IdenticalCount,
			},
		},
		{
			id:    "sensor." + prefix + "_archive_newest",
			state: timestamp(data.ArchiveNewestAt),
			attributes: map[string]any{
				"friendly_name": "Timelapse newest archived image",
				"device_class":  "timestamp",
				"icon":          "mdi:image-check",
				"filename":      data.ArchiveNewest,
			},
		},
		{
			id:    "sensor." + prefix + "_spool_files",
			state: fmt.Sprint(in.Spool.Files),
			attributes: map[string]any{
				"friendly_name":       "Timelapse buffered images",
				"unit_of_measurement": "images",
				"state_class":         "measurement",
				"icon":                "mdi:tray-full",
				"bytes":               in.Spool.Bytes,
			},
		},
		{
			id:    "sensor." + prefix + "_status",
			state: overallStatus(in),
			attributes: map[string]any{
				"friendly_name":        "Timelapse status",
				"icon":                 "mdi:camera-timer",
				"version":              in.Version,
				"capture_enabled":      in.CaptureEnabled,
				"captures_ok":          data.CapturesOK,
				"captures_failed":      data.CapturesFailed,
				"consecutive_failures": data.ConsecutiveFailures,
				"last_capture":         timestamp(data.LastCaptureSuccess),
				"last_sync":            timestamp(data.LastSyncSuccess),
				"last_error":           data.LastCaptureError,
				"next_capture":         timestampPtr(in.NextCapture),
			},
		},
	}

	if in.CaptureEnabled {
		entities = append(entities, entity{
			id:    "sensor." + prefix + "_last_capture",
			state: timestamp(data.LastCaptureSuccess),
			attributes: map[string]any{
				"friendly_name": "Timelapse last capture",
				"device_class":  "timestamp",
				"icon":          "mdi:camera",
				"filename":      latestFilename(data),
			},
		})
	}
	return entities
}

// overallStatus collapses the detail into one word, which is what an automation
// usually wants to trigger on.
func overallStatus(in Input) string {
	data := in.Data
	switch {
	case in.CaptureEnabled && data.ConsecutiveFailures > 0:
		return "failing"
	case !data.CameraOnline && !data.LastProbeAt.IsZero():
		return "camera_offline"
	case data.FrameFrozen:
		return "frozen"
	case !in.ArchiveAvailable && in.Spool.Files > 0:
		return "buffering"
	default:
		return "ok"
	}
}

func (p *Publisher) write(ctx context.Context, e entity) error {
	payload, err := json.Marshal(map[string]any{
		"state":      e.state,
		"attributes": e.attributes,
	})
	if err != nil {
		return err
	}

	endpoint := p.cfg.BaseURL + "/api/states/" + e.id
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// timestamp renders an instant in the RFC 3339 form Home Assistant expects for
// a timestamp device class, and "unknown" when nothing has happened yet.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

func timestampPtr(t *time.Time) string {
	if t == nil {
		return "unknown"
	}
	return timestamp(*t)
}

func latestFilename(d state.Data) string {
	if d.Latest == nil {
		return ""
	}
	return d.Latest.Filename
}

// SanitizePrefix reduces a configured prefix to characters that are valid in a
// Home Assistant entity id.
func SanitizePrefix(prefix string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(prefix) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "timelapse"
	}
	return b.String()
}
