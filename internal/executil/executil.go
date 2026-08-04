package executil

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/gitexec"
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
// and Run's default. Git delegates to gitexec, which additionally enforces its
// non-interactive subprocess environment.
func RunEnv(ctx context.Context, dir string, env []string, name string, args ...string) error {
	if name == "git" {
		return gitexec.Run(ctx, gitexec.Options{Dir: dir, Env: env}, args...)
	}
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
	if name == "git" {
		return gitexec.Output(ctx, gitexec.Options{Dir: dir}, args...)
	}
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
