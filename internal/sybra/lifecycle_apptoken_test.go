package sybra

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
)

// TestMintAppTokenBeforeRecovery_DisabledIsNoop pins the "no App auth
// configured" branch: nothing is enabled and no token is minted, matching
// startAppAuthLoop's prior no-op behavior for this case.
func TestMintAppTokenBeforeRecovery_DisabledIsNoop(t *testing.T) {
	t.Cleanup(github.DisableAppAuth)
	cfg := config.DefaultConfig()
	a := &App{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	lm := newLifecycleManager(a)

	lm.mintAppTokenBeforeRecovery(context.Background())

	if github.AppAuthEnabled() {
		t.Fatal("App auth should stay disabled when github.app.enabled=false")
	}
}

// TestMintAppTokenBeforeRecovery_InvalidCredsDegradesNonBlocking covers
// #2494's "degrade to ambient credentials on failure instead of blocking
// startup" requirement: a misconfigured App (unreadable private key) must
// fail fast, log, and leave gh on its own ambient auth rather than wedging
// the synchronous pre-RunStartupCleanup mint call.
func TestMintAppTokenBeforeRecovery_InvalidCredsDegradesNonBlocking(t *testing.T) {
	t.Cleanup(github.DisableAppAuth)
	cfg := config.DefaultConfig()
	cfg.GitHub.App.Enabled = true
	cfg.GitHub.App.AppID = 42
	cfg.GitHub.App.InstallationID = 7
	cfg.GitHub.App.PrivateKeyPath = filepath.Join(t.TempDir(), "does-not-exist.pem")
	a := &App{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	lm := newLifecycleManager(a)

	start := time.Now()
	lm.mintAppTokenBeforeRecovery(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("mintAppTokenBeforeRecovery took %v for a local key-load failure, want near-instant", elapsed)
	}

	if github.AppAuthEnabled() {
		t.Fatal("App auth should stay disabled after a mint-config failure (degrade to ambient credentials)")
	}
	if github.CurrentAppToken() != "" {
		t.Fatal("no token should be cached after a failed mint")
	}
}

// TestStartAppAuthLoop_NoopWhenNeverEnabled pins the split between the
// synchronous startup mint (mintAppTokenBeforeRecovery) and the periodic
// renewal ticker (startAppAuthLoop, now called from StartManagers): when App
// auth was never successfully enabled, the ticker must not be registered at
// all — it must not re-attempt EnableAppAuth or re-mint on its own.
func TestStartAppAuthLoop_NoopWhenNeverEnabled(t *testing.T) {
	t.Cleanup(github.DisableAppAuth)
	cfg := config.DefaultConfig()
	a := &App{cfg: cfg, logger: slog.New(slog.DiscardHandler)}
	lm := newLifecycleManager(a)

	lm.startAppAuthLoop(t.Context())

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startAppAuthLoop registered a goroutine despite App auth never being enabled")
	}
}

// TestAppStartup_AppAuthMintFailureDoesNotBlockStartup exercises the full
// App.Startup path with GitHub App auth misconfigured: Startup must still
// complete (and complete quickly), proving the synchronous bounded-timeout
// mint added ahead of RunStartupCleanup (see startLifecycle) cannot wedge
// boot on a mint outage.
func TestAppStartup_AppAuthMintFailureDoesNotBlockStartup(t *testing.T) {
	preventFetchTTLLeak(t)
	t.Cleanup(github.DisableAppAuth)
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	t.Setenv("SYBRA_DISABLE_WORKFLOWS", "0")

	cfg := startupTestConfig(home)
	cfg.GitHub.App.Enabled = true
	cfg.GitHub.App.AppID = 42
	cfg.GitHub.App.InstallationID = 7
	cfg.GitHub.App.PrivateKeyPath = filepath.Join(home, "does-not-exist.pem")

	app := NewApp(slog.New(slog.DiscardHandler), &slog.LevelVar{}, cfg)

	start := time.Now()
	if err := app.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Startup took %v with App auth misconfigured, want a fast non-blocking degrade", elapsed)
	}
	t.Cleanup(func() {
		if app.agentSvc != nil && app.agentSvc.approval != nil {
			_ = app.agentSvc.approval.Shutdown(context.Background())
		}
		app.Shutdown(context.Background())
	})

	if github.AppAuthEnabled() {
		t.Fatal("App auth should stay disabled after Startup when the configured key cannot be loaded")
	}
}
