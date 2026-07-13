package agent

import (
	"log/slog"
	"testing"
)

func newPostureManager(mode string) *Manager {
	return &Manager{defaultSandboxMode: mode, logger: slog.New(slog.DiscardHandler)}
}

func TestInjectProcessSandbox_UnsetModeInheritsConfiguredPosture(t *testing.T) {
	m := newPostureManager("enforce")

	cfg := &RunConfig{}
	_ = m.injectProcessSandbox(cfg)

	if cfg.SandboxMode != "enforce" {
		t.Fatalf("SandboxMode = %q, want enforce: a dispatch path that leaves SandboxMode unset "+
			"must inherit the operator's configured posture, not silently fall back to report and "+
			"run the agent unsandboxed", cfg.SandboxMode)
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
