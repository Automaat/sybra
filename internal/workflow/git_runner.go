package workflow

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// gitCmd builds a git subprocess bound to ctx and rooted at dir, with the
// same forceful-cancel wiring every workflow git call needs (see
// configureWorkflowGitCancel). Every git invocation in this package goes
// through this constructor (directly or via gitStdout/gitCombinedOutput/
// gitDo/gitOK) so that wiring can never be forgotten on a new call site —
// before this, only one of ~40 call sites applied it.
func gitCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	configureWorkflowGitCancel(cmd)
	return cmd
}

// gitCmdEnv is gitCmd for the rare call site that must override the
// subprocess environment (e.g. a scratch GIT_INDEX_FILE for a throwaway tree
// build) rather than inheriting the caller's.
func gitCmdEnv(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := gitCmd(ctx, dir, args...)
	cmd.Env = env
	return cmd
}

// configureWorkflowGitCancel makes a git subprocess's context cancellation
// SIGKILL the whole process group instead of relying on the default
// os.Process.Kill (which only signals the direct child): git can spawn
// helper processes (e.g. a credential helper) that would otherwise survive
// a canceled ctx and keep the worktree locked.
func configureWorkflowGitCancel(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return err
		}
		return nil
	}
}

// gitStdout runs a git command and returns its trimmed stdout. The wrapped
// error includes stderr (exec.Cmd.Output captures it into the ExitError when
// Stderr is unset) so callers get the same diagnostic detail a
// CombinedOutput call would.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitCmd(ctx, dir, args...).Output()
	if err != nil {
		return "", wrapGitOutputErr(args, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func wrapGitOutputErr(args []string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
	}
	return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// gitCombinedOutput runs a git command and returns the raw combined
// stdout+stderr bytes alongside a wrapped error, for callers that must
// inspect output text even on failure (e.g. flake/retry classification).
func gitCombinedOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	out, err := gitCmd(ctx, dir, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return out, nil
}

// gitDo runs a git command for its exit code/side effect only, wrapping any
// failure with its combined output. A best-effort caller that doesn't care
// about the outcome discards the error explicitly (`_ = gitDo(...)`).
func gitDo(ctx context.Context, dir string, args ...string) error {
	_, err := gitCombinedOutput(ctx, dir, args...)
	return err
}

// gitOK runs a git command and reports only whether it exited zero, for
// git's own boolean-by-exit-code checks (ref verify, is-ancestor, diff
// --quiet) where the caller has no use for stdout/stderr.
func gitOK(ctx context.Context, dir string, args ...string) bool {
	return gitCmd(ctx, dir, args...).Run() == nil
}

// gitRevParseVerify resolves rev via `git rev-parse --verify` and reports
// whether it exists.
func gitRevParseVerify(ctx context.Context, dir, rev string) (sha string, ok bool) {
	sha, err := gitStdout(ctx, dir, "rev-parse", "--verify", rev)
	return sha, err == nil
}

// gitIsAncestor reports whether ancestor is reachable from descendant, per
// `git merge-base --is-ancestor`'s exit-code contract (0 = yes, 1 = no). A
// caller that must distinguish "no" from a genuine command failure (e.g. an
// unresolvable ref) should use gitCmd directly instead.
func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	return gitOK(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
}
