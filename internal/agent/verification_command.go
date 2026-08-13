package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// RunVerificationCommand runs one deterministic verification command through
// the same sandbox preparation and process wrapper used for provider CLIs.
// The caller must supply a disposable writable checkout as cfg.Dir.
func (m *Manager) RunVerificationCommand(ctx context.Context, cfg RunConfig, name string, args []string, output io.Writer) (runErr error) {
	if cfg.Role != RoleTestRunner || strings.TrimSpace(cfg.TaskID) == "" {
		return fmt.Errorf("verification command requires a task-scoped test-runner role")
	}
	if err := validateRunDir(cfg.Dir); err != nil {
		return err
	}
	sandboxHome, err := os.MkdirTemp("", "sybra-verify-scratch-")
	if err != nil {
		return fmt.Errorf("verification command: create ephemeral sandbox home: %w", err)
	}
	defer func() {
		if cleanupErr := removeVerificationHome(sandboxHome); cleanupErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("verification command: remove ephemeral sandbox home: %w", cleanupErr))
		}
	}()
	cfg.EphemeralSandboxHome = sandboxHome
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

func removeVerificationHome(root string) error {
	// Do not walk and chmod repository-controlled paths: a concurrent symlink
	// swap could escape the scratch root. RemoveAll stays rooted by name, and
	// its error is propagated by the caller instead of silently leaking state.
	return os.RemoveAll(root)
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
