//go:build linux

package agent

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var (
	bwrapPathOnce sync.Once
	bwrapPath     string
)

func sandboxExecAvailable() bool {
	bwrapPathOnce.Do(func() {
		if p, err := exec.LookPath("bwrap"); err == nil {
			bwrapPath = p
		}
	})
	return bwrapPath != ""
}

func materializeSandboxProfile() (string, error) {
	return "", nil
}

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

func wrapInvocation(name string, args []string, cfg *RunConfig) (wrappedName string, wrappedArgs []string) {
	if cfg == nil || cfg.sandbox.mode != "enforce" {
		return name, args
	}
	wrapped := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	roots := dedupeRoots(
		cfg.sandbox.worktree, cfg.sandbox.sandboxHome, cfg.sandbox.tmp, cfg.sandbox.sharedCache,
		cfg.sandbox.claudeState, cfg.sandbox.codexState, cfg.sandbox.copilotState, cfg.sandbox.toolCache,
	)
	for _, root := range roots {
		wrapped = append(wrapped, "--bind", root, root)
	}
	wrapped = append(wrapped, "--", name)
	wrapped = append(wrapped, args...)
	return bwrapPath, wrapped
}

func dedupeRoots(roots ...string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
