package workflow

import (
	"regexp"
	"strings"
	"testing"
)

// sidecarRefPattern finds every scratch-file reference in a builtin workflow,
// whichever directory expression it is anchored to.
var sidecarRefPattern = regexp.MustCompile(`\{\{[^}]*\}\}/\.sybra-[a-z-]+-`)

// TestVerifierWorkflowsWriteOutsideTheWorktree guards the invariant the
// read-only gate depends on. Verifier roles run against a worktree they cannot
// write (#2791), so every scratch file they produce must be anchored to the
// sidecar directory. A path that drifts back to _dir would not fail loudly —
// the agent's write would be denied by the sandbox and the review would come
// back empty — so it is pinned here instead.
func TestVerifierWorkflowsWriteOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	// Both workflows dispatch roles for which Role.JudgesWithoutWriting holds.
	for _, name := range []string{"builtin/simple-task-review.yaml", "builtin/simple-task-plan.yaml"} {
		data, err := builtinFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(data)
		refs := sidecarRefPattern.FindAllString(body, -1)
		if len(refs) == 0 {
			t.Fatalf("%s: found no scratch-file references; has the naming changed?", name)
		}
		for _, ref := range refs {
			if !strings.Contains(ref, "sidecardir") {
				t.Errorf("%s: scratch file anchored to %q, want the sidecar dir — a read-only worktree cannot accept this write", name, ref)
			}
		}

		// A bare relative reference is the other way this regresses: the agent
		// would resolve it against its cwd, which is the read-only worktree.
		for line := range strings.SplitSeq(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, bare := range []string{" .sybra-", "\t.sybra-", "(.sybra-"} {
				if strings.Contains(line, bare) {
					t.Errorf("%s: bare relative scratch path in %q — resolves against the read-only worktree", name, trimmed)
				}
			}
		}
	}
}
