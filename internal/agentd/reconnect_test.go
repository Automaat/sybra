package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestRegistrationWaitsForLeaderWithSameIdentity(t *testing.T) {
	var attempts atomic.Int32
	var identity string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req workercontrol.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if identity == "" {
			identity = req.RegistrationID
		}
		if req.RegistrationID != identity {
			t.Error("registration identity changed during transport retry")
		}
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(workercontrol.Session{SessionID: "session-reconnected", State: "active"})
	}))
	defer server.Close()
	d := registrationTestDaemon(t, server.URL)
	if err := d.retryRegistration(t.Context(), time.Millisecond, 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 || d.spool.snapshot().SessionID != "session-reconnected" {
		t.Fatalf("attempts=%d session=%q", attempts.Load(), d.spool.snapshot().SessionID)
	}
}

func TestRegistrationRefusesPermanentAuthFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	d := registrationTestDaemon(t, server.URL)
	if err := d.retryRegistration(t.Context(), time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("unauthorized registration succeeded")
	}
	if attempts.Load() != 1 {
		t.Fatalf("permanent failure retried %d times", attempts.Load())
	}
}

func TestRegistrationRefusesUntrustedLeaderCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	d := registrationTestDaemon(t, server.URL)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := d.retryRegistration(ctx, time.Millisecond, time.Millisecond)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || retryableLeaderError(err) {
		t.Fatalf("untrusted certificate was not rejected permanently: %v", err)
	}
}

func TestRegistrationBackoffCancelsPromptly(t *testing.T) {
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		called <- struct{}{}
	}))
	defer server.Close()
	d := registrationTestDaemon(t, server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.retryRegistration(ctx, time.Hour, time.Hour) }()
	<-called
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backoff ignored cancellation")
	}
}

func registrationTestDaemon(t *testing.T, endpoint string) *Daemon {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AGENTD_RECONNECT_TOKEN", "fixture")
	d, err := New(t.Context(), Config{
		LeaderURL: endpoint, TokenEnv: "AGENTD_RECONNECT_TOKEN", NodeID: "reconnect-test", Capacity: 1,
		Providers: []string{providerid.Claude}, SandboxMode: "report",
		WorkspaceRoot: filepath.Join(root, "workspaces"), StateRoot: filepath.Join(root, "state"),
		SpoolMaxBytes: 1 << 20,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
