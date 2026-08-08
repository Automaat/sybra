package agent

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// RunVerificationCommand runs one deterministic verification command through
// the same sandbox preparation and process wrapper used for provider CLIs.
// The caller must supply a disposable writable checkout as cfg.Dir.
func (m *Manager) RunVerificationCommand(ctx context.Context, cfg RunConfig, name string, args []string, output io.Writer) error {
	if cfg.Role != RoleTestRunner || strings.TrimSpace(cfg.TaskID) == "" {
		return fmt.Errorf("verification command requires a task-scoped test-runner role")
	}
	if err := validateRunDir(cfg.Dir); err != nil {
		return err
	}
	cfg.EphemeralSandboxHome = filepath.Join(filepath.Dir(cfg.Dir), "scratch")
	cfg.DisableVerifierControl = true
	if err := m.injectSandboxHome(&cfg); err != nil {
		return err
	}
	if err := isolateVerifierGitCredentials(&cfg); err != nil {
		return err
	}
	if err := m.injectGolangciCache(&cfg); err != nil {
		return err
	}
	if err := m.injectSharedBuildCache(&cfg); err != nil {
		return err
	}
	cfg.SandboxMode = "enforce"
	cfg.SandboxReadMode = "enforce"
	if err := m.injectProcessSandbox(&cfg); err != nil { //nolint:contextcheck // sandbox Git discovery intentionally uses the manager lifecycle context, matching provider dispatch.
		return err
	}
	if err := m.certifyPreparedCommand(ctx, cfg); err != nil {
		return err
	}
	cmd := newProviderCmd(ctx, &cfg, false, name, args...)
	cmd.Dir = cfg.Dir
	cmd.Env = cfg.ExtraEnv
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func (m *Manager) certifyPreparedCommand(ctx context.Context, cfg RunConfig) error {
	m.mu.RLock()
	preflight := m.runEnvironmentPreflight
	m.mu.RUnlock()
	if preflight == nil {
		return nil
	}
	return preflight(ctx, RunEnvironment{
		TaskID: cfg.TaskID, Role: cfg.Role, Dir: cfg.Dir, SandboxMode: cfg.SandboxMode,
		ScratchRoots: preparedScratchRoots(cfg), LocalCommand: true,
	})
}
