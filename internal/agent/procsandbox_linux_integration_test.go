//go:build linux

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

func TestSandboxEnforce_FencesWritesToAllowlist(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("bwrap not installed; enforce path unexercised on this host")
	}
	wt, err := canonicalizeRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    wt,
		sandboxHome: wt,
		tmp:         wt,
		sharedCache: wt,
	}}
	leak := filepath.Join(os.TempDir(), "sybra-enforce-leak-probe")
	_ = os.Remove(leak)
	t.Cleanup(func() { _ = os.Remove(leak) })

	script := "touch " + wt + "/inside && echo INSIDE_OK; " +
		"(touch " + leak + " 2>/dev/null && echo LEAK) || echo OUTSIDE_DENIED"
	cmd := newProviderCmd(context.Background(), cfg, false, "sh", "-c", script)
	out, runErr := cmd.CombinedOutput()
	got := string(out)

	if !strings.Contains(got, "INSIDE_OK") {
		t.Errorf("write to worktree root should succeed (err=%v): %q", runErr, got)
	}
	if strings.Contains(got, "LEAK") || !strings.Contains(got, "OUTSIDE_DENIED") {
		t.Errorf("write outside the allowlist must be kernel-denied: %q", got)
	}
	if _, statErr := os.Stat(leak); statErr == nil {
		t.Errorf("leak probe %q exists — sandbox did not fence the write", leak)
	}
	if _, statErr := os.Stat(filepath.Join(wt, "inside")); statErr != nil {
		t.Errorf("in-sandbox file not created: %v", statErr)
	}
}

func TestSandboxEnforce_LinkedWorktreeGitOps(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("bwrap not installed; enforce path unexercised on this host")
	}

	h := newSandboxGitHarness(t)
	h.advanceUpstreamMain(t)
	siblingAdmin := h.gitPath(t, h.siblingWt, "--git-dir")
	siblingBranchRef := h.gitRefPath(t, h.siblingWt)
	siblingWant := strings.TrimSpace(h.git(t, h.siblingWt, "rev-parse", "fix/sibling"))
	h.gitBare(t, h.sybraBare, "pack-refs", "--all")
	if err := os.Remove(siblingBranchRef); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove loose sibling branch ref: %v", err)
	}
	if _, err := os.Stat(siblingBranchRef); !os.IsNotExist(err) {
		t.Fatalf("sibling loose branch ref still exists after setup: %v", err)
	}

	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return h.sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-linked-git",
		Mode:        "headless",
		Dir:         h.taskWt,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.sandbox.gitAdminDir == "" || cfg.sandbox.gitCommonDir == "" || cfg.sandbox.gitWorktrees == "" {
		t.Fatalf("expected git sandbox roots, got %+v", cfg.sandbox)
	}

	script := strings.Join([]string{
		"set -e",
		"echo task > task.txt",
		"git add task.txt",
		"git commit -m 'task change'",
		"git fetch origin",
		"git merge --no-edit origin/main",
		"git push -u origin HEAD",
		`echo "GIT_OK"`,
	}, "\n")
	cmd := newProviderCmd(context.Background(), &cfg, false, "bash", "-lc", script)
	cmd.Dir = h.taskWt
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed git ops: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "GIT_OK") {
		t.Fatalf("sandboxed git ops output missing marker:\n%s", out)
	}

	localHead := strings.TrimSpace(h.git(t, h.taskWt, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(h.gitBare(t, h.remoteBare, "rev-parse", "refs/heads/fix/task"))
	if localHead != remoteHead {
		t.Fatalf("remote head = %s, want local head %s", remoteHead, localHead)
	}
	parents := strings.Fields(strings.TrimSpace(h.git(t, h.taskWt, "rev-list", "--parents", "-1", "HEAD")))
	if len(parents) != 3 {
		t.Fatalf("merge commit parent list = %v, want 3 fields (commit + 2 parents)", parents)
	}
	status := strings.TrimSpace(h.git(t, h.taskWt, "status", "--porcelain"))
	if status != "" {
		t.Fatalf("worktree not clean after sandboxed git ops: %q", status)
	}
	if body := h.git(t, h.taskWt, "show", "--format=%B", "--no-patch", "HEAD"); !strings.Contains(body, "Signed-off-by: Test <test@test.com>") {
		t.Fatalf("merge commit body missing signoff:\n%s", body)
	}

	outside := filepath.Join(h.base, "outside-probe")
	denyScript := strings.Join([]string{
		"set -e",
		"(touch " + bashQuote(filepath.Join(h.siblingWt, "leak.txt")) + " 2>/dev/null && echo SIBLING_WT_LEAK) || echo SIBLING_WT_DENIED",
		"(touch " + bashQuote(filepath.Join(siblingAdmin, "leak.lock")) + " 2>/dev/null && echo SIBLING_GIT_LEAK) || echo SIBLING_GIT_DENIED",
		"(echo hacked > " + bashQuote(siblingBranchRef) + " 2>/dev/null && echo SIBLING_REF_WRITE_LOCAL) || echo SIBLING_REF_DENIED",
		"(touch " + bashQuote(outside) + " 2>/dev/null && echo OUTSIDE_LEAK) || echo OUTSIDE_DENIED",
	}, "\n")
	denyCmd := newProviderCmd(context.Background(), &cfg, false, "bash", "-lc", denyScript)
	denyCmd.Dir = h.taskWt
	denyOut, denyErr := denyCmd.CombinedOutput()
	if denyErr != nil {
		t.Fatalf("sandbox denial probe: %v\n%s", denyErr, denyOut)
	}
	got := string(denyOut)
	for _, want := range []string{"SIBLING_WT_DENIED", "SIBLING_GIT_DENIED", "OUTSIDE_DENIED"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sandbox denial output missing %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "SIBLING_REF_WRITE_LOCAL") && !strings.Contains(got, "SIBLING_REF_DENIED") {
		t.Fatalf("sandbox sibling ref probe produced no expected marker:\n%s", got)
	}
	for _, leaked := range []string{
		filepath.Join(h.siblingWt, "leak.txt"),
		filepath.Join(siblingAdmin, "leak.lock"),
		outside,
	} {
		if _, err := os.Stat(leaked); err == nil {
			t.Fatalf("unexpected leaked file %q", leaked)
		}
	}
	if _, err := os.Stat(siblingBranchRef); !os.IsNotExist(err) {
		t.Fatalf("sandbox recreated sibling loose branch ref on host: %v", err)
	}
	if gotRef := strings.TrimSpace(h.git(t, h.siblingWt, "rev-parse", "fix/sibling")); gotRef != siblingWant {
		t.Fatalf("sibling branch changed unexpectedly: got %s want %s", gotRef, siblingWant)
	}
}

func TestPrepareRunConfig_Sandbox_EnforceFailsClosedOnBrokenGitMetadata(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("bwrap not installed; enforce path unexercised on this host")
	}

	h := newSandboxGitHarness(t)
	dotGit := filepath.Join(h.taskWt, ".git")
	if err := os.WriteFile(dotGit, []byte("gitdir: /nonexistent/sybra-git-admin\n"), 0o644); err != nil {
		t.Fatalf("break .git: %v", err)
	}

	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return h.sandboxHome, nil },
	})
	_, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-broken-git",
		Mode:        "headless",
		Dir:         h.taskWt,
		SandboxMode: "enforce",
	})
	if err == nil {
		t.Fatal("expected broken linked-worktree git metadata to fail closed")
	}
	if !strings.Contains(err.Error(), "sandbox git metadata roots") {
		t.Fatalf("error = %v, want sandbox git metadata roots context", err)
	}
}

type sandboxGitHarness struct {
	base        string
	sandboxHome string
	remoteBare  string
	src         string
	sybraBare   string
	taskWt      string
	siblingWt   string
}

func newSandboxGitHarness(t *testing.T) sandboxGitHarness {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	base, err := os.MkdirTemp(wd, ".sybra-bwrap-*")
	if err != nil {
		t.Fatalf("MkdirTemp(wd): %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	h := sandboxGitHarness{
		base:        base,
		sandboxHome: t.TempDir(),
		src:         filepath.Join(base, "src"),
		sybraBare:   filepath.Join(base, "sybra.git"),
		taskWt:      filepath.Join(base, "task-wt"),
		siblingWt:   filepath.Join(base, "sibling-wt"),
	}
	h.remoteBare = filepath.Join(h.sandboxHome, "remote.git")

	h.gitRaw(t, "", "init", "--bare", h.remoteBare)
	h.gitRaw(t, "", "init", "-b", "main", h.src)
	h.gitRaw(t, h.src, "config", "user.email", "test@test.com")
	h.gitRaw(t, h.src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(h.src, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	h.gitRaw(t, h.src, "add", ".")
	h.gitRaw(t, h.src, "commit", "-m", "init")
	h.gitRaw(t, h.src, "remote", "add", "origin", h.remoteBare)
	h.gitRaw(t, h.src, "push", "-u", "origin", "main")

	if err := project.CloneBare(context.Background(), h.remoteBare, h.sybraBare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	if err := project.CreateWorktree(context.Background(), h.sybraBare, h.taskWt, "fix/task", "main"); err != nil {
		t.Fatalf("CreateWorktree(task): %v", err)
	}
	if err := project.CreateWorktree(context.Background(), h.sybraBare, h.siblingWt, "fix/sibling", "main"); err != nil {
		t.Fatalf("CreateWorktree(sibling): %v", err)
	}
	for _, dir := range []string{h.taskWt, h.siblingWt} {
		h.gitRaw(t, dir, "config", "user.email", "test@test.com")
		h.gitRaw(t, dir, "config", "user.name", "Test")
	}
	return h
}

func (h sandboxGitHarness) advanceUpstreamMain(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.src, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatalf("write upstream.txt: %v", err)
	}
	h.gitRaw(t, h.src, "add", ".")
	h.gitRaw(t, h.src, "commit", "-m", "advance main")
	h.gitRaw(t, h.src, "push", "origin", "main")
}

func (h sandboxGitHarness) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return h.gitRaw(t, dir, args...)
}

func (h sandboxGitHarness) gitBare(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return h.gitRaw(t, dir, append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

func (h sandboxGitHarness) gitPath(t *testing.T, dir, arg string) string {
	t.Helper()
	out := strings.TrimSpace(h.git(t, dir, "rev-parse", arg))
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return out
}

func (h sandboxGitHarness) gitRefPath(t *testing.T, dir string) string {
	t.Helper()
	ref := strings.TrimSpace(h.git(t, dir, "symbolic-ref", "-q", "HEAD"))
	path := strings.TrimSpace(h.git(t, dir, "rev-parse", "--git-path", ref))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return path
}

func (h sandboxGitHarness) gitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func bashQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
