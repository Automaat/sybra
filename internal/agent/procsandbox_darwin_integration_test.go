//go:build darwin

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxEnforce_GrantsGitAdminDirWrite reproduces task 24849431's
// EPERM: a linked worktree's gitdir (HEAD, index, logs/HEAD) lives outside
// WORKTREE, under the shared bare clone's own worktrees/<branch>/
// subdirectory. Runs real sandbox-exec: a write inside GIT_ADMIN_DIR must
// succeed, while a write to a sibling path under the same parent (standing
// in for the shared bare-clone root or another task's worktree) must still
// be kernel-denied — the isolation property the narrow grant depends on.
func TestSandboxEnforce_GrantsGitAdminDirWrite(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	profilePath, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile: %v", err)
	}

	wt, err := canonicalizeRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clonesRoot, err := canonicalizeRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitAdminDir := filepath.Join(clonesRoot, "worktrees", "this-task-branch")
	if err := os.MkdirAll(gitAdminDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Stands in for the shared bare-clone root / a sibling task's own admin
	// dir: same parent as GIT_ADMIN_DIR, but never granted.
	sibling := filepath.Join(clonesRoot, "worktrees", "other-task-branch")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    wt,
		sandboxHome: wt,
		tmp:         wt,
		sharedCache: wt,
		gitAdminDir: gitAdminDir,
		profilePath: profilePath,
	}}

	script := "touch " + filepath.Join(gitAdminDir, "HEAD") + " && echo ADMIN_OK; " +
		"(touch " + filepath.Join(sibling, "HEAD") + " 2>/dev/null && echo SIBLING_LEAK) || echo SIBLING_DENIED"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, runErr := cmd.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, "ADMIN_OK") {
		t.Errorf("write to GIT_ADMIN_DIR should succeed (err=%v): %q", runErr, got)
	}
	if strings.Contains(got, "SIBLING_LEAK") || !strings.Contains(got, "SIBLING_DENIED") {
		t.Errorf("write to a sibling worktree admin dir must be kernel-denied: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(gitAdminDir, "HEAD")); statErr != nil {
		t.Errorf("in-sandbox file not created: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(sibling, "HEAD")); statErr == nil {
		t.Errorf("sibling leak probe exists — sandbox did not fence the write")
	}
}
