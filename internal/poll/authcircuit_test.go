package poll

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuthCircuit_OpensAfterThreshold(t *testing.T) {
	t.Parallel()
	c := NewAuthCircuit("test", discardLogger())
	err := errors.New("bad credentials")

	for i := 0; i < AuthFailureThreshold-1; i++ {
		c.RecordFailure(err)
		if c.Open() {
			t.Fatalf("circuit opened after %d failures, want threshold %d", i+1, AuthFailureThreshold)
		}
	}

	c.RecordFailure(err)
	if !c.Open() {
		t.Fatalf("circuit did not open after %d consecutive failures", AuthFailureThreshold)
	}
}

func TestAuthCircuit_RecordSuccessResets(t *testing.T) {
	t.Parallel()
	c := NewAuthCircuit("test", discardLogger())
	err := errors.New("bad credentials")

	for i := 0; i < AuthFailureThreshold; i++ {
		c.RecordFailure(err)
	}
	if !c.Open() {
		t.Fatal("expected circuit to be open before success")
	}

	c.RecordSuccess()
	if c.Open() {
		t.Fatal("expected circuit to close after RecordSuccess")
	}

	// One more failure after a reset should not reopen the breaker — the
	// counter must have been zeroed, not just the open flag.
	c.RecordFailure(err)
	if c.Open() {
		t.Fatal("circuit reopened on a single failure after reset — consecutive count wasn't cleared")
	}
}

func TestHub_AuthHealthSnapshot(t *testing.T) {
	t.Parallel()
	healthy := &fakeAuthFetcher{name: "healthy"}
	critical := &fakeAuthFetcher{name: "critical", open: true}
	plain := &fakeFetcher{name: "plain"}

	hub := NewHub()
	hub.Register(healthy, 0)
	hub.Register(critical, 0)
	hub.Register(plain, 0)

	snapshot := hub.AuthHealthSnapshot()
	if got := snapshot["healthy"]; got != 1 {
		t.Errorf("healthy poller = %d, want 1", got)
	}
	if got := snapshot["critical"]; got != 0 {
		t.Errorf("critical poller = %d, want 0", got)
	}
	if _, ok := snapshot["plain"]; ok {
		t.Errorf("plain fetcher (no AuthHealthReporter) should be excluded, got entry")
	}
}

type fakeAuthFetcher struct {
	name string
	open bool
}

func (f *fakeAuthFetcher) Name() string                         { return f.name }
func (f *fakeAuthFetcher) Poll(_ context.Context) time.Duration { return time.Minute }
func (f *fakeAuthFetcher) AuthCircuitOpen() bool                { return f.open }

type fakeFetcher struct {
	name string
}

func (f *fakeFetcher) Name() string                         { return f.name }
func (f *fakeFetcher) Poll(_ context.Context) time.Duration { return time.Minute }
