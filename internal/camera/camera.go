// Package camera fetches still images from a camera. Two sources are supported:
// the camera's own anonymous snapshot endpoint, and the UniFi Protect
// integration API.
package camera

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/steiner-dominik/unifi-protect-timelapse/internal/config"
)

// maxImageBytes caps how much of a response we are willing to buffer. Protect
// snapshots are well under a megabyte; this only exists so that a misconfigured
// URL pointing at something huge cannot exhaust memory.
const maxImageBytes = 32 << 20

// ErrNotAnImage reports that the endpoint answered, but not with a JPEG.
var ErrNotAnImage = errors.New("response is not a JPEG image")

// Source fetches a single still image.
type Source interface {
	// Snapshot returns the raw JPEG bytes of a fresh frame.
	Snapshot(ctx context.Context) ([]byte, error)
	// Describe returns a short human-readable description of the source,
	// free of any secret material.
	Describe() string
}

// New builds the Source named by CAMERA_SOURCE, without retries or fallback.
func New(cfg *config.Config, log *slog.Logger) (Source, error) {
	return newSource(cfg, cfg.Camera.Kind, log)
}

// Build assembles the complete snapshot pipeline: the primary source with its
// retries and, when configured, a fallback source with its own retries behind
// it. This is what the service uses; New exists for callers that want one bare
// source.
func Build(cfg *config.Config, log *slog.Logger) (Source, error) {
	primary, err := newSource(cfg, cfg.Camera.Kind, log)
	if err != nil {
		return nil, fmt.Errorf("primary camera source: %w", err)
	}
	pipeline := withRetries(cfg, primary, log)

	if cfg.Camera.Fallback == "" {
		return pipeline, nil
	}

	fallback, err := newSource(cfg, cfg.Camera.Fallback, log)
	if err != nil {
		return nil, fmt.Errorf("fallback camera source: %w", err)
	}
	return NewChain(pipeline, withRetries(cfg, fallback, log), log), nil
}

func withRetries(cfg *config.Config, source Source, log *slog.Logger) Source {
	return &Retrying{
		Source:   source,
		Attempts: cfg.Camera.Retries,
		Delay:    cfg.Camera.RetryDelay,
		Log:      log,
	}
}

func newSource(cfg *config.Config, kind config.SourceKind, log *slog.Logger) (Source, error) {
	client := &http.Client{
		Timeout: cfg.Camera.Timeout,
		// Snapshot endpoints should not be redirecting anywhere; following a
		// redirect off-host would be a way to smuggle the request elsewhere.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}

	// Applies to every source. Cameras present self-signed certificates just as
	// consoles do, and this previously only covered the Protect source, so the
	// setting appeared to do nothing for an HTTPS snapshot URL.
	if cfg.Camera.InsecureTLS {
		log.Warn("TLS certificate verification is disabled for camera requests",
			"source", kind,
			"hint", "unset CAMERA_INSECURE_TLS once the endpoint presents a trusted certificate")
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in, self-signed camera and console certificates
		}
	}

	switch kind {
	case config.SourceSnapshot:
		return &snapshotSource{client: client, url: cfg.Camera.SnapshotURL, cfg: cfg.Camera}, nil
	case config.SourceProtect:
		return &protectSource{client: client, cfg: cfg.Camera}, nil
	default:
		return nil, fmt.Errorf("unsupported camera source %q", kind)
	}
}

// fetch performs the request and validates that the answer really is a JPEG of
// plausible size. The original shell script wrote whatever came back to disk,
// so an HTTP error page was archived as a .jpg; these checks are what prevent
// that.
func fetch(ctx context.Context, client *http.Client, req *http.Request, minBytes int64) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); !strings.HasPrefix(strings.TrimSpace(mediaType), "image/") {
			return nil, fmt.Errorf("%w: content-type %q", ErrNotAnImage, mediaType)
		}
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(data)) < minBytes {
		return nil, fmt.Errorf("%w: only %d bytes, expected at least %d", ErrNotAnImage, len(data), minBytes)
	}
	if !isJPEG(data) {
		return nil, fmt.Errorf("%w: missing JPEG signature", ErrNotAnImage)
	}
	return data, nil
}

// isJPEG checks the SOI marker every JPEG file starts with.
func isJPEG(data []byte) bool {
	return len(data) >= 3 && bytes.Equal(data[:3], []byte{0xFF, 0xD8, 0xFF})
}

// Retrying wraps a Source with bounded retries and a fixed backoff.
type Retrying struct {
	Source
	Attempts int
	Delay    time.Duration
	Log      *slog.Logger
}

// Snapshot retries the wrapped source until it succeeds or the attempts are
// exhausted. The context is honoured between attempts so shutdown is prompt.
func (r *Retrying) Snapshot(ctx context.Context) ([]byte, error) {
	attempts := max(r.Attempts, 1)

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		data, err := r.Source.Snapshot(ctx)
		if err == nil {
			if attempt > 1 && r.Log != nil {
				r.Log.Info("snapshot succeeded after retry", "attempt", attempt)
			}
			return data, nil
		}
		lastErr = err
		if attempt == attempts {
			break
		}
		if r.Log != nil {
			r.Log.Warn("snapshot attempt failed, retrying",
				"attempt", attempt, "of", attempts, "delay", r.Delay, "error", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.Delay):
		}
	}
	return nil, fmt.Errorf("all %d attempts failed: %w", attempts, lastErr)
}
