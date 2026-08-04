// Package gitexec is the single low-level boundary for production Git
// subprocess execution. It owns process mechanics only; repository policy
// such as locking, authentication, retries, and reconciliation belongs to
// higher-level packages.
package gitexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const terminalPromptEnv = "GIT_TERMINAL_PROMPT=0"

// Options configures one Git invocation.
//
// A nil Env inherits the current process environment. A non-nil Env, including
// an empty slice, is used as the complete base environment. ExtraEnv overlays
// either base, which supports inherited, augmented, and fully isolated
// subprocess environments without changing the process environment.
// GIT_TERMINAL_PROMPT=0 is applied last for every invocation.
type Options struct {
	Dir      string
	Env      []string
	ExtraEnv []string
	Stdin    io.Reader
}

// Run executes Git for its side effects and returns a diagnostic error that
// includes combined output on failure.
func Run(ctx context.Context, opts Options, args ...string) error {
	_, err := CombinedOutput(ctx, opts, args...)
	return err
}

// Output executes Git and returns trimmed stdout. Failures include captured
// stderr in the returned diagnostic error.
func Output(ctx context.Context, opts Options, args ...string) (string, error) {
	out, err := output(ctx, opts, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// RawOutput executes Git and returns stdout without trimming it. This is for
// byte-sensitive output such as patches; most callers should use Output.
func RawOutput(ctx context.Context, opts Options, args ...string) ([]byte, error) {
	return output(ctx, opts, args...)
}

// CombinedOutput executes Git and returns raw interleaved stdout and stderr.
// The output is returned even on failure so callers can classify Git errors.
func CombinedOutput(ctx context.Context, opts Options, args ...string) ([]byte, error) {
	cmd := command(ctx, opts, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, commandError(args, err, out)
	}
	return out, nil
}

// ExitCode returns the subprocess exit code contained in err. It understands
// errors wrapped by this package; ok is false for start failures and errors
// that do not originate from a completed subprocess.
func ExitCode(err error) (code int, ok bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

func output(ctx context.Context, opts Options, args ...string) ([]byte, error) {
	cmd := command(ctx, opts, args...)
	out, err := cmd.Output()
	if err != nil {
		var detail []byte
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail = exitErr.Stderr
		}
		return out, commandError(args, err, detail)
	}
	return out, nil
}

func command(ctx context.Context, opts Options, args ...string) *exec.Cmd {
	name := "git"
	if path := usablePathExecutable(name); path != "" {
		name = path
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = opts.Dir
	cmd.Env = invocationEnv(opts)
	cmd.Stdin = opts.Stdin
	configureProcessGroupCancel(cmd)
	return cmd
}

func invocationEnv(opts Options) []string {
	var env []string
	if opts.Env == nil {
		env = append([]string(nil), os.Environ()...)
	} else {
		env = append([]string(nil), opts.Env...)
	}
	for _, entry := range opts.ExtraEnv {
		env = overlayEnv(env, entry)
	}
	return overlayEnv(env, terminalPromptEnv)
}

func overlayEnv(env []string, entry string) []string {
	key, _, ok := strings.Cut(entry, "=")
	if !ok {
		return append(env, entry)
	}
	out := env[:0]
	for _, existing := range env {
		existingKey, _, existingOK := strings.Cut(existing, "=")
		if !existingOK || existingKey != key {
			out = append(out, existing)
		}
	}
	return append(out, entry)
}

func commandError(args []string, err error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
}

// usablePathExecutable skips dangling or non-executable PATH entries instead
// of accepting the first textual match. Long-lived Sybra processes can retain
// a stale profile-manager shim after an upgrade while a usable system Git
// remains later on PATH.
func usablePathExecutable(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, name)
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && executableByCurrentUser(resolved, info) {
			return resolved
		}
	}
	return ""
}
