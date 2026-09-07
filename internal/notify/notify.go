// Package notify sends failure notifications to a generic webhook. The payload
// is plain JSON so it works with Home Assistant webhooks, ntfy, Gotify,
// Discord and anything else that accepts a POST.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// Event is the JSON body posted to the webhook.
type Event struct {
	Service   string    `json:"service"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// Notifier posts events to the configured webhook. When no webhook is
// configured every call is a no-op, so callers need no conditionals.
type Notifier struct {
	url    string
	client *http.Client
	log    *slog.Logger

	// mu guards the de-duplication state, so a persistent failure does not
	// produce one notification per interval forever.
	mu       sync.Mutex
	lastSent map[string]time.Time
}

// resendAfter is how long the same kind of event is suppressed for.
const resendAfter = time.Hour

// New returns a Notifier for cfg.
func New(cfg config.Notify, log *slog.Logger) *Notifier {
	return &Notifier{
		url:      cfg.WebhookURL,
		client:   &http.Client{Timeout: cfg.Timeout},
		log:      log,
		lastSent: make(map[string]time.Time),
	}
}

// Enabled reports whether a webhook is configured.
func (n *Notifier) Enabled() bool { return n.url != "" }

// Send posts an event, suppressing repeats of the same kind within the resend
// window. It never returns an error: a notification failure must not affect
// capturing.
func (n *Notifier) Send(ctx context.Context, event Event) {
	if !n.Enabled() {
		return
	}

	n.mu.Lock()
	if last, ok := n.lastSent[event.Kind]; ok && time.Since(last) < resendAfter {
		n.mu.Unlock()
		return
	}
	n.lastSent[event.Kind] = time.Now()
	n.mu.Unlock()

	event.Service = "unifi-protect-timelapse"
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	body, err := json.Marshal(event)
	if err != nil {
		n.log.Warn("encoding notification failed", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		n.log.Warn("building notification request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Warn("sending notification failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		n.log.Warn("notification endpoint returned an error", "status", resp.Status)
		return
	}
	n.log.Debug("notification sent", "kind", event.Kind)
}

// Resolve clears the suppression for a kind, so the next occurrence notifies
// again. Call it when the underlying condition has recovered.
func (n *Notifier) Resolve(kind string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.lastSent, kind)
}
