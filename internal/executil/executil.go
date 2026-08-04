package executil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EscapeAppleScript escapes a string for safe embedding inside an AppleScript
// double-quoted string literal. It escapes backslashes first, then double quotes.
func EscapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// Run executes a command in dir, returning a formatted error with stderr on failure.
func Run(ctx context.Context, dir, name string, args ...string) error {
	return RunEnv(ctx, dir, nil, name, args...)
}

// RunEnv executes a command in dir with an explicit environment, returning a
// formatted error with stderr on failure. A nil env means "inherit the
// current process environment", matching exec.Cmd's own zero-value behavior
// and Run's default.
func RunEnv(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// Output executes a command in dir and returns its trimmed stdout.
func Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// commandContext avoids exec.LookPath accepting a stale executable symlink.
// This matters for long-lived desktop processes after a profile-manager
// upgrade: a now-dangling git shim can remain first on PATH even though a
// usable system git follows it. Worktree preparation must not fail before the
// provider sandbox is even reached.
func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if name == "git" {
		if path := usablePathExecutable(name); path != "" {
			return exec.CommandContext(ctx, path, args...)
		}
	}
	return exec.CommandContext(ctx, name, args...)
}

func usablePathExecutable(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, name)
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return resolved
		}
	}
	return ""
}
