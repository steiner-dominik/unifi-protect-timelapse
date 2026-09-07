package camera

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Chain tries a primary source and falls back to a secondary one when the
// primary fails. The primary is attempted on every call, so the better source
// is always preferred and recovery needs no timer: the cost of a failing
// primary is bounded by its own timeout and retry budget.
type Chain struct {
	Primary    Source
	Fallback   Source
	Log        *slog.Logger
	OnFallback func(err error)

	mu       sync.Mutex
	lastUsed string
}

// NewChain returns a Chain. When fallback is nil the primary is returned
// unwrapped, so a deployment without a fallback pays nothing for the feature.
func NewChain(primary, fallback Source, log *slog.Logger) Source {
	if fallback == nil {
		return primary
	}
	return &Chain{Primary: primary, Fallback: fallback, Log: log}
}

// Snapshot returns a frame from the primary source, or from the fallback if the
// primary could not produce one.
func (c *Chain) Snapshot(ctx context.Context) ([]byte, error) {
	data, primaryErr := c.Primary.Snapshot(ctx)
	if primaryErr == nil {
		c.setLastUsed("primary")
		return data, nil
	}
	// A cancelled context is the caller shutting down, not a camera problem;
	// trying the fallback would only produce a second identical error.
	if ctx.Err() != nil {
		return nil, primaryErr
	}

	if c.Log != nil {
		c.Log.Warn("primary camera source failed, trying the fallback",
			"primary", c.Primary.Describe(),
			"fallback", c.Fallback.Describe(),
			"error", primaryErr)
	}
	if c.OnFallback != nil {
		c.OnFallback(primaryErr)
	}

	data, fallbackErr := c.Fallback.Snapshot(ctx)
	if fallbackErr != nil {
		c.setLastUsed("none")
		return nil, fmt.Errorf("primary failed (%w) and fallback failed: %w", primaryErr, fallbackErr)
	}

	c.setLastUsed("fallback")
	return data, nil
}

// Describe names both sources.
func (c *Chain) Describe() string {
	return c.Primary.Describe() + " (fallback: " + c.Fallback.Describe() + ")"
}

// LastUsed reports which source last produced a frame: "primary", "fallback",
// "none", or "" before the first attempt. It feeds the status page and metrics
// so a silent degradation to the lesser source is visible.
func (c *Chain) LastUsed() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUsed
}

func (c *Chain) setLastUsed(v string) {
	c.mu.Lock()
	c.lastUsed = v
	c.mu.Unlock()
}

// SourceReporter is implemented by sources that can say which underlying
// endpoint served the most recent frame.
type SourceReporter interface {
	LastUsed() string
}

// LastUsed reports the active endpoint for any source, treating a plain
// single-endpoint source as always primary.
func LastUsed(s Source) string {
	if r, ok := s.(SourceReporter); ok {
		return r.LastUsed()
	}
	return "primary"
}
