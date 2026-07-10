//go:build darwin

package agent

// This file wraps provider CLI subprocesses in an OS-level sandbox-exec
// seatbelt profile so a write outside the agent's worktree/sandbox-home/tmp
// is denied by the kernel (see #1578, #1576). It is unrelated to
// internal/sandbox, which provisions Docker/k3d containers for an
// application *under test* by a task, not the agent process itself.

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed agent_sandbox.sb
var agentSandboxProfile []byte

var (
	sandboxExecPathOnce sync.Once
	sandboxExecPath     string

	sandboxProfileOnce sync.Once
	sandboxProfilePath string
	errSandboxProfile  error
)

// sandboxExecAvailable reports whether sandbox-exec is on PATH. Every
// supported macOS release ships it at /usr/bin/sandbox-exec, but a stripped
// CI image or a future OS removal must be detected rather than assumed.
func sandboxExecAvailable() bool {
	sandboxExecPathOnce.Do(func() {
		if p, err := exec.LookPath("sandbox-exec"); err == nil {
			sandboxExecPath = p
		}
	})
	return sandboxExecPath != ""
}

// materializeSandboxProfile writes the embedded seatbelt profile to a stable
// temp file once per process and returns its path, for both the -f flag and
// operator-facing log/error messages. The profile content is fixed at build
// time, so re-writing it more than once per process would only waste I/O.
func materializeSandboxProfile() (string, error) {
	sandboxProfileOnce.Do(func() {
		f, err := os.CreateTemp("", "sybra-agent-sandbox-*.sb")
		if err != nil {
			errSandboxProfile = fmt.Errorf("write sandbox profile: %w", err)
			return
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write(agentSandboxProfile); err != nil {
			errSandboxProfile = fmt.Errorf("write sandbox profile: %w", err)
			return
		}
		sandboxProfilePath = f.Name()
	})
	return sandboxProfilePath, errSandboxProfile
}

// canonicalizeRoot resolves root to an absolute, symlink-free path suitable
// for templating into a seatbelt -D value. sandbox-exec matches subpaths
// literally against the resolved filesystem, so an un-resolved /tmp (a
// symlink to /private/tmp on darwin) would deny every legitimate write
// under it in enforce mode, and evaluating symlinks/".." here (rather than
// trusting the caller) is what makes a crafted or stale root incapable of
// escaping the intended allowlist.
func canonicalizeRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("sandbox: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve root %q: %w", root, err)
	}
	return resolved, nil
}

// wrapInvocation transposes name/args into a sandbox-exec invocation when
// cfg carries an enforce-mode sandbox spec (set by
// Manager.injectProcessSandbox). report/off specs are never wrapped here:
// report mode's spec is validated and logged at dispatch time but
// intentionally never reaches this function with mode=="enforce", so a
// profile or SBPL defect can only affect enforce-mode runs, never the
// default (report) posture — see the rollback note on DefaultSandboxMode. A
// nil cfg (tests constructing a command directly) is never wrapped.
func wrapInvocation(name string, args []string, cfg *RunConfig) (wrappedName string, wrappedArgs []string) {
	if cfg == nil || cfg.sandbox.mode != "enforce" {
		return name, args
	}
	wrapped := make([]string, 0, len(args)+11)
	wrapped = append(wrapped,
		"-f", cfg.sandbox.profilePath,
		"-D", "WORKTREE="+cfg.sandbox.worktree,
		"-D", "SANDBOX_HOME="+cfg.sandbox.sandboxHome,
		"-D", "TMP="+cfg.sandbox.tmp,
		"-D", "SHARED_CACHE="+cfg.sandbox.sharedCache,
		name,
	)
	wrapped = append(wrapped, args...)
	return sandboxExecPath, wrapped
}
