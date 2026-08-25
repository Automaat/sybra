package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/Automaat/sybra/internal/llmexec"
)

// OneShotCommand builds a sandboxed process for a one-shot provider call.
//
// A classifier or judge needs none of an agent run's machinery — no worktree,
// no board credential, no git roots — but it runs the same provider CLI on the
// same host, so it takes the same containment. dir stands in for the worktree:
// it is the only project-shaped path the call can write, and callers pass a
// scratch directory rather than a checkout.
//
// The returned func removes the ephemeral sandbox home, and the caller must
// call it once the process has exited. Removal is deliberately not tied to
// ctx: callers pass the app's root context, which is done only at shutdown,
// so one home per classification would accumulate for the life of the server.
func (m *Manager) OneShotCommand(ctx context.Context, dir, name string, args []string) (*exec.Cmd, func(), error) {
	home, err := os.MkdirTemp("", "sybra-oneshot-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("agent.OneShotCommand: create sandbox home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	// provider identifies the binary so an enforce-mode spawn resolves a
	// symlinked launcher to its real target, the way agent dispatch does.
	// DisableVerifierControl is deliberately not set: it doubles as "this is
	// not a provider CLI" and would strip the provider's own state roots out
	// of the write allowlist, leaving the call pointed at the operator's real
	// ~/.claude and ~/.codex with no permission to write them.
	cfg := RunConfig{
		Dir:                 dir,
		resolvedSandboxHome: home,
		provider:            providerByName(name),
	}
	if err := injectScratchEnvironment(&cfg); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := m.injectShellTempPrefix(&cfg); err != nil {
		cleanup()
		return nil, nil, err
	}
	// A one-shot call is a classifier, not an actor. It is given no board and
	// no GitHub credential, so a prompt-injected instruction inside the text
	// it classifies has nothing to read them out of.
	cfg.ExtraEnv = stripEnvKeys(cfg.ExtraEnv, "SYBRA_HOME", "GH_TOKEN", "GITHUB_TOKEN", "SYBRA_AUTH_TOKEN", "SYBRA_AUTH_TOKEN_FILE", "SYBRA_CONTROL_HOME", "SYBRA_SERVER_TARGET")
	cfg.ExtraEnv = append(cfg.ExtraEnv,
		"SYBRA_HOME="+home,
		"GH_TOKEN=",
		"GITHUB_TOKEN=",
		"SYBRA_AUTH_TOKEN=",
		"SYBRA_AUTH_TOKEN_FILE=",
		"SYBRA_CONTROL_HOME=",
		"SYBRA_SERVER_TARGET=",
	)
	//nolint:contextcheck // sandbox Git discovery intentionally uses the manager lifecycle context, matching provider dispatch.
	if err := m.injectProcessSandbox(&cfg); err != nil {
		cleanup()
		return nil, nil, err
	}
	cmd := newProviderCmd(ctx, &cfg, false, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	return cmd, cleanup, nil
}

// RegisterOneShotCommands routes every later llmexec call through this
// manager's sandbox. Without it those calls spawn a provider CLI with no wrap,
// no scratch home, and the serving process's own working directory (#3383).
func (m *Manager) RegisterOneShotCommands() {
	llmexec.SetCommandFactory(m.OneShotCommand)
}
