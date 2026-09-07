package camera

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSource struct {
	data  []byte
	err   error
	calls int
	name  string
}

func (f *fakeSource) Snapshot(context.Context) ([]byte, error) {
	f.calls++
	return f.data, f.err
}
func (f *fakeSource) Describe() string { return f.name }

func TestChainPrefersPrimary(t *testing.T) {
	primary := &fakeSource{data: []byte("primary"), name: "primary"}
	fallback := &fakeSource{data: []byte("fallback"), name: "fallback"}

	chain := NewChain(primary, fallback, discardLogger()).(*Chain)

	data, err := chain.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if string(data) != "primary" {
		t.Errorf("got %q, want the primary source's frame", data)
	}
	if fallback.calls != 0 {
		t.Error("the fallback must not be touched while the primary works")
	}
	if chain.LastUsed() != "primary" {
		t.Errorf("LastUsed = %q", chain.LastUsed())
	}
}

func TestChainFallsBackWhenPrimaryFails(t *testing.T) {
	primary := &fakeSource{err: errors.New("console unreachable"), name: "primary"}
	fallback := &fakeSource{data: []byte("fallback"), name: "fallback"}

	chain := NewChain(primary, fallback, discardLogger()).(*Chain)

	data, err := chain.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if string(data) != "fallback" {
		t.Errorf("got %q, want the fallback source's frame", data)
	}
	if chain.LastUsed() != "fallback" {
		t.Errorf("LastUsed = %q", chain.LastUsed())
	}
}

// The chosen behaviour is to retry the primary on every capture, so recovery is
// automatic and needs no cooldown timer.
func TestChainRetriesPrimaryEveryTime(t *testing.T) {
	primary := &fakeSource{err: errors.New("down"), name: "primary"}
	fallback := &fakeSource{data: []byte("fallback"), name: "fallback"}
	chain := NewChain(primary, fallback, discardLogger()).(*Chain)

	for range 3 {
		if _, err := chain.Snapshot(context.Background()); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
	}
	if primary.calls != 3 {
		t.Errorf("primary was tried %d times, want 3", primary.calls)
	}

	// Once the primary recovers it is used again with no further prompting.
	primary.err = nil
	primary.data = []byte("primary")

	data, err := chain.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if string(data) != "primary" {
		t.Errorf("got %q, want the recovered primary", data)
	}
	if chain.LastUsed() != "primary" {
		t.Errorf("LastUsed = %q after recovery", chain.LastUsed())
	}
}

func TestChainReportsBothErrorsWhenEverythingFails(t *testing.T) {
	primary := &fakeSource{err: errors.New("console unreachable"), name: "primary"}
	fallback := &fakeSource{err: errors.New("camera unreachable"), name: "fallback"}
	chain := NewChain(primary, fallback, discardLogger()).(*Chain)

	_, err := chain.Snapshot(context.Background())
	if err == nil {
		t.Fatal("expected an error when both sources fail")
	}
	for _, want := range []string{"console unreachable", "camera unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if chain.LastUsed() != "none" {
		t.Errorf("LastUsed = %q", chain.LastUsed())
	}
}

// Shutting down must not be mistaken for a primary failure worth falling back
// over; the fallback would only produce the same cancellation.
func TestChainDoesNotFallBackOnCancellation(t *testing.T) {
	primary := &fakeSource{err: context.Canceled, name: "primary"}
	fallback := &fakeSource{data: []byte("fallback"), name: "fallback"}
	chain := NewChain(primary, fallback, discardLogger()).(*Chain)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := chain.Snapshot(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if fallback.calls != 0 {
		t.Error("the fallback must not run for a cancelled context")
	}
}

// Without a fallback the chain must not wrap anything, so a simple deployment
// pays nothing for the feature.
func TestNewChainWithoutFallbackReturnsPrimary(t *testing.T) {
	primary := &fakeSource{name: "primary"}
	if got := NewChain(primary, nil, discardLogger()); got != Source(primary) {
		t.Error("expected the primary source to be returned unwrapped")
	}
	if LastUsed(primary) != "primary" {
		t.Errorf("LastUsed on a plain source = %q", LastUsed(primary))
	}
}
