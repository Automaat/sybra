package workflow

import (
	"context"

	"github.com/Automaat/sybra/internal/gitexec"
)

// gitStdout runs a Git command and returns its trimmed stdout. Process-group
// cancellation and failure diagnostics are provided by the shared boundary.
func gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	return gitexec.Output(ctx, gitexec.Options{Dir: dir}, args...)
}

// gitCombinedOutput returns raw interleaved stdout and stderr, including the
// output on failure for workflow retry and failure classification.
func gitCombinedOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitexec.CombinedOutput(ctx, gitexec.Options{Dir: dir}, args...)
}

// gitDo runs a Git command for its side effect and preserves combined failure
// diagnostics. Best-effort callers discard the returned error explicitly.
func gitDo(ctx context.Context, dir string, args ...string) error {
	return gitexec.Run(ctx, gitexec.Options{Dir: dir}, args...)
}

// gitOK reports only whether Git exited zero for boolean-by-exit-code checks.
func gitOK(ctx context.Context, dir string, args ...string) bool {
	return gitexec.RunQuiet(ctx, gitexec.Options{Dir: dir}, args...) == nil
}

// gitRevParseVerify resolves rev via `git rev-parse --verify` and reports
// whether it exists.
func gitRevParseVerify(ctx context.Context, dir, rev string) (sha string, ok bool) {
	sha, err := gitStdout(ctx, dir, "rev-parse", "--verify", rev)
	return sha, err == nil
}

// gitIsAncestor reports whether ancestor is reachable from descendant, per
// `git merge-base --is-ancestor`'s exit-code contract (0 = yes, 1 = no).
func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	return gitOK(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
}
