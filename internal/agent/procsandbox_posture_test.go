package agent

import (
	"log/slog"
	"strings"
	"testing"
)

func newPostureManager(mode string) *Manager {
	return &Manager{defaultSandboxMode: mode, logger: slog.New(slog.DiscardHandler)}
}

func TestInjectProcessSandbox_UnsetModeInheritsConfiguredPosture(t *testing.T) {
	m := newPostureManager("enforce")

	cfg := &RunConfig{Dir: t.TempDir()}
	err := m.injectProcessSandbox(cfg)
	if err != nil && !strings.Contains(err.Error(), "enforce sandbox mode requires sandbox-exec") {
		t.Fatalf("injectProcessSandbox: %v", err)
	}

	if cfg.SandboxMode != "enforce" {
		t.Fatalf("SandboxMode = %q, want enforce: a dispatch path that leaves SandboxMode unset "+
			"must inherit the operator's configured posture, not silently fall back to report and "+
			"run the agent unsandboxed", cfg.SandboxMode)
	}
}

func TestReplaceRuntimeConfigUpdatesDefaultSandboxPosture(t *testing.T) {
	m := newPostureManager("report")

	if err := m.ReplaceRuntimeConfig(ManagerRuntimeConfig{SandboxMode: "enforce"}); err != nil {
		t.Fatalf("ReplaceRuntimeConfig: %v", err)
	}

	cfg := &RunConfig{Dir: t.TempDir()}
	err := m.injectProcessSandbox(cfg)
	if err != nil && !strings.Contains(err.Error(), "enforce sandbox mode requires sandbox-exec") {
		t.Fatalf("injectProcessSandbox: %v", err)
	}
	if cfg.SandboxMode != "enforce" {
		t.Fatalf("SandboxMode = %q, want enforce after live config reload", cfg.SandboxMode)
	}
}

func TestInjectProcessSandbox_ExplicitModeWinsOverDefault(t *testing.T) {
	m := newPostureManager("enforce")

	cfg := &RunConfig{SandboxMode: "off"}
	if err := m.injectProcessSandbox(cfg); err != nil {
		t.Fatalf("injectProcessSandbox: %v", err)
	}

	if cfg.SandboxMode != "off" {
		t.Errorf("SandboxMode = %q, want off: an explicit per-task opt-out must still win", cfg.SandboxMode)
	}
	if cfg.sandbox.mode != "off" {
		t.Errorf("sandbox.mode = %q, want off", cfg.sandbox.mode)
	}
}
