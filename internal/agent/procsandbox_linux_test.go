//go:build linux

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWrapInvocation_Linux_EnforceBindsOnlyWriteRoots(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:                   "enforce",
		worktree:               "/data/wt",
		gitMetadata:            []string{"/data/clones/repo.git/worktrees/task", "/data/clones/repo.git"},
		gitReadonly:            []string{"/data/clones/repo.git/objects"},
		sandboxHome:            "/data/home",
		sidecarDir:             "/data/tasks/task-1/sidecars",
		tmp:                    "/tmp",
		sharedCache:            "/data/cache",
		gitAdminDir:            "/data/clones/repo.git/worktrees/task",
		gitCommonDir:           "/data/clones/repo.git",
		gitWorktrees:           "/data/clones/repo.git/worktrees",
		gitObjectDir:           "/data/clones/repo.git/objects",
		claudeState:            "/data/sybra/claude",
		codexState:             "/data/sybra/codex",
		copilotState:           "/data/sybra/copilot",
		opencodeState:          "/data/sybra/opencode",
		toolCache:              "/home/sybra/.cache",
		gitBranchRef:           "refs/heads/fix/task",
		gitBranchRefDir:        "/data/clones/repo.git/refs/heads/fix",
		gitBranchLogDir:        "/data/clones/repo.git/logs/refs/heads/fix",
		gitRemoteRefDir:        "/data/clones/repo.git/refs/remotes",
		gitRemoteLogDir:        "/data/clones/repo.git/logs/refs/remotes",
		gitTagRefDir:           "/data/clones/repo.git/refs/tags",
		gitTagLogDir:           "/data/clones/repo.git/logs/refs/tags",
		gitOverlayObjectDir:    "/data/home/.sybra-git-overlay/objects",
		gitOverlayRefDir:       "/data/home/.sybra-git-overlay/refs",
		gitOverlayLogDir:       "/data/home/.sybra-git-overlay/logs",
		gitOverlayRefFile:      "/data/home/.sybra-git-overlay/refs/task",
		gitOverlayRemoteRefDir: "/data/home/.sybra-git-overlay/remote-refs",
		gitOverlayRemoteLogDir: "/data/home/.sybra-git-overlay/remote-logs",
		gitOverlayTagRefDir:    "/data/home/.sybra-git-overlay/tag-refs",
		gitOverlayTagLogDir:    "/data/home/.sybra-git-overlay/tag-logs",
	}}
	name, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	if name != "/bin/sh" {
		t.Fatalf("overlay sync wrapper name = %q, want /bin/sh", name)
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--unshare-pid --ro-bind / / --dev /dev --proc /proc") {
		t.Fatalf("expected pid-namespace isolation + read-only root + fresh dev/proc bwrap args, got: %s", joined)
	}
	for _, root := range []string{
		"/data/wt",
		"/data/home", "/data/tasks/task-1/sidecars", "/tmp", "/data/cache",
		"/data/sybra/claude", "/data/sybra/codex", "/data/sybra/copilot", "/data/sybra/opencode", "/home/sybra/.cache",
	} {
		if !strings.Contains(joined, "--bind "+root+" "+root) {
			t.Errorf("write root %q not bound read-write: %s", root, joined)
		}
	}
	if !strings.Contains(joined, "--ro-bind /data/clones/repo.git/objects /data/clones/repo.git/objects") {
		t.Fatalf("shared object dir must stay read-only: %s", joined)
	}
	if strings.Contains(joined, "--bind /data/clones/repo.git /data/clones/repo.git") {
		t.Fatalf("shared git common dir must not be reopened read-write: %s", joined)
	}
	if strings.Contains(joined, "--bind /data/clones/repo.git/objects /data/clones/repo.git/objects") {
		t.Fatalf("shared object dir must not be reopened read-write: %s", joined)
	}
	if strings.Contains(joined, "--bind /data/clones/repo.git/refs/heads/fix /data/clones/repo.git/refs/heads/fix") {
		t.Fatalf("shared branch ref dir must not be reopened read-write: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/clones/repo.git/worktrees/task /data/clones/repo.git/worktrees/task") {
		t.Fatalf("task git admin dir must be reopened read-write: %s", joined)
	}
	if !strings.Contains(joined, "sync_git_objects") || !strings.Contains(joined, shellQuote("/data/home/.sybra-git-overlay/objects")) || !strings.Contains(joined, shellQuote("/data/clones/repo.git/objects")) {
		t.Fatalf("sync wrapper must merge task-private objects back into the shared clone: %s", joined)
	}
	for _, path := range []string{
		"/data/home/.sybra-git-overlay/objects/pack",
		"/data/home/.sybra-git-overlay/objects/info",
	} {
		if !strings.Contains(joined, "--ro-bind "+path+" "+path) {
			t.Fatalf("maintenance surface %q must be re-locked read-only: %s", path, joined)
		}
	}
	if strings.Contains(joined, `for pack in "$src"/pack/pack-*`) {
		t.Fatalf("sync wrapper must never publish task-private pack files: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind /data/clones/repo.git/worktrees /data/clones/repo.git/worktrees") {
		t.Fatalf("shared worktrees dir must remain read-only: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/refs /data/clones/repo.git/refs/heads/fix") {
		t.Fatalf("task branch ref overlay must cover the shared branch dir: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/logs /data/clones/repo.git/logs/refs/heads/fix") {
		t.Fatalf("task branch log overlay must cover the shared branch log dir: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/remote-refs /data/clones/repo.git/refs/remotes") {
		t.Fatalf("remote refs must be isolated behind a task-private overlay: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/remote-logs /data/clones/repo.git/logs/refs/remotes") {
		t.Fatalf("remote logs must be isolated behind a task-private overlay: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/tag-refs /data/clones/repo.git/refs/tags") {
		t.Fatalf("tag refs must be isolated behind a task-private overlay: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/home/.sybra-git-overlay/tag-logs /data/clones/repo.git/logs/refs/tags") {
		t.Fatalf("tag logs must be isolated behind a task-private overlay: %s", joined)
	}
	for _, root := range []string{
		"/data/clones/repo.git/refs/remotes",
		"/data/clones/repo.git/logs/refs/remotes",
		"/data/clones/repo.git/refs/tags",
		"/data/clones/repo.git/logs/refs/tags",
	} {
		if strings.Contains(joined, "--bind "+root+" "+root) {
			t.Fatalf("shared git refs dir %q must not be reopened read-write: %s", root, joined)
		}
	}
	if !strings.Contains(joined, "update-ref 'refs/heads/fix/task'") {
		t.Fatalf("sync wrapper must update only the current branch ref: %s", joined)
	}
	if !strings.Contains(joined, `base_ref=$(cat '/data/home/.sybra-git-overlay/refs/task'`) ||
		!strings.Contains(joined, `elif [ "$new_ref" != "$base_ref" ]; then`) ||
		!strings.Contains(joined, `"$new_ref" "$base_ref"`) {
		t.Fatalf("sync wrapper must skip no-op ref writes and CAS real updates: %s", joined)
	}
	sep := slices.Index(args, "--")
	if sep < 0 || sep+1 >= len(args) || args[sep+1] != "claude" {
		t.Fatalf("provider must follow the -- separator, got: %s", joined)
	}
	if !slices.Equal(args[sep+1:], []string{"claude", "-p", "hi"}) {
		t.Errorf("provider argv altered: %v", args[sep+1:])
	}
}

func TestWrapInvocation_Linux_DetachedHeadStillSyncsObjects(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode: "enforce", worktree: "/data/verification/source", sandboxHome: "/data/verification/scratch",
		gitObjectDir: "/data/verification/source/.git/objects", gitOverlayObjectDir: "/data/verification/scratch/.sybra-git-overlay/objects",
	}}
	name, args := wrapInvocation("git", []string{"commit", "-m", "fixture"}, cfg)
	if name != "/bin/sh" {
		t.Fatalf("detached-head object sync wrapper = %q, want /bin/sh", name)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "sync_git_objects") {
		t.Fatalf("detached-head command does not publish overlay objects: %s", joined)
	}
}

func TestSandboxSyncShell_UnchangedRefPreservesReflog(t *testing.T) {
	repo := t.TempDir()
	runGitOrFail(t, repo, "init", "-q", "-b", "main")
	runGitOrFail(t, repo, "config", "user.email", "t@example.com")
	runGitOrFail(t, repo, "config", "user.name", "T")
	runGitOrFail(t, repo, "config", "commit.gpgsign", "false")
	runGitOrFail(t, repo, "commit", "-q", "--allow-empty", "-m", "seed")

	gitOutput := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	head := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
	overlay := t.TempDir()
	refFile := filepath.Join(overlay, "refs", "main")
	if err := os.MkdirAll(filepath.Dir(refFile), 0o700); err != nil {
		t.Fatalf("create overlay ref dir: %v", err)
	}
	if err := os.WriteFile(refFile, []byte(head+"\n"), 0o600); err != nil {
		t.Fatalf("write overlay ref: %v", err)
	}

	reflogs := func() string {
		return gitOutput("reflog", "show", "--format=%H%x00%gD%x00%gs", "HEAD") +
			gitOutput("reflog", "show", "--format=%H%x00%gD%x00%gs", "refs/heads/main")
	}
	before := reflogs()
	name, args := sandboxSyncShell(nil, &RunConfig{sandbox: sandboxSpec{
		worktree:            repo,
		gitObjectDir:        filepath.Join(repo, ".git", "objects"),
		gitOverlayObjectDir: filepath.Join(overlay, "objects"),
		gitOverlayRefFile:   refFile,
		gitBranchRef:        "refs/heads/main",
	}})
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("resolve true: %v", err)
	}
	// sandboxSyncShell passes the sandbox launcher as argv[3]. Replacing it
	// with true exercises only the post-command publisher, without requiring
	// bwrap or mutating its package-global probe state.
	if len(args) <= 3 {
		t.Fatalf("sync wrapper argv = %v, want launcher at index 3", args)
	}
	args[3] = truePath
	cmd := exec.CommandContext(t.Context(), name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run sync wrapper: %v: %s", err, out)
	}
	if after := reflogs(); after != before {
		t.Fatalf("no-op sandbox publish changed ref history\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSandboxSyncShell_UnchangedOverlayDoesNotRewindConcurrentRefAdvance(t *testing.T) {
	repo := t.TempDir()
	runGitOrFail(t, repo, "init", "-q", "-b", "main")
	runGitOrFail(t, repo, "config", "user.email", "t@example.com")
	runGitOrFail(t, repo, "config", "user.name", "T")
	runGitOrFail(t, repo, "config", "commit.gpgsign", "false")
	runGitOrFail(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base := strings.TrimSpace(runGitOutputOrFail(t, repo, "rev-parse", "HEAD"))
	runGitOrFail(t, repo, "commit", "-q", "--allow-empty", "-m", "concurrent")
	concurrent := strings.TrimSpace(runGitOutputOrFail(t, repo, "rev-parse", "HEAD"))
	runGitOrFail(t, repo, "reset", "-q", "--hard", base)

	overlay := t.TempDir()
	refFile := filepath.Join(overlay, "refs", "main")
	if err := os.MkdirAll(filepath.Dir(refFile), 0o700); err != nil {
		t.Fatalf("create overlay ref dir: %v", err)
	}
	if err := os.WriteFile(refFile, []byte(base+"\n"), 0o600); err != nil {
		t.Fatalf("write overlay ref: %v", err)
	}

	advance := "git -C " + shellQuote(repo) + " update-ref refs/heads/main " + shellQuote(concurrent) + " " + shellQuote(base)
	name, args := sandboxSyncShell([]string{"-c", advance}, &RunConfig{sandbox: sandboxSpec{
		worktree:            repo,
		gitObjectDir:        filepath.Join(repo, ".git", "objects"),
		gitOverlayObjectDir: filepath.Join(overlay, "objects"),
		gitOverlayRefFile:   refFile,
		gitBranchRef:        "refs/heads/main",
	}})
	if len(args) <= 3 {
		t.Fatalf("sync wrapper argv = %v, want launcher at index 3", args)
	}
	args[3] = "/bin/sh"
	cmd := exec.CommandContext(t.Context(), name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run sync wrapper: %v: %s", err, out)
	}
	if got := strings.TrimSpace(runGitOutputOrFail(t, repo, "rev-parse", "refs/heads/main")); got != concurrent {
		t.Fatalf("branch ref = %s, want concurrent advance %s (overlay baseline %s must not rewind it)", got, concurrent, base)
	}
}

func runGitOutputOrFail(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// TestInjectProcessSandbox_ReadOnlyDirNeverBindsDirWritable pins the
// human-review fix: a run whose Dir is a diagnostic-only checkout (e.g. the
// Sybra deploy/build source, not a task worktree) must never get that Dir
// added to the sandbox's writable roots under enforce, so the default
// `--ro-bind / /` keeps it read-only regardless of RequirePermissions.
func TestInjectProcessSandbox_ReadOnlyDirNeverBindsDirWritable(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("bwrap not installed; enforce path unexercised on this host")
	}
	m := newPostureManager("enforce")

	dir := t.TempDir()
	cfg := &RunConfig{TaskID: "task-1", Dir: dir, ReadOnlyDir: true, resolvedSandboxHome: t.TempDir()}
	if err := m.injectProcessSandbox(cfg); err != nil {
		t.Fatalf("injectProcessSandbox: %v", err)
	}
	if cfg.sandbox.mode != "enforce" {
		t.Fatalf("sandbox.mode = %q, want enforce", cfg.sandbox.mode)
	}
	if cfg.sandbox.worktree != "" {
		t.Fatalf("sandbox.worktree = %q, want empty: ReadOnlyDir must never register Dir as a writable root", cfg.sandbox.worktree)
	}
	canonDir, err := canonicalizeRoot(dir)
	if err != nil {
		t.Fatalf("canonicalizeRoot(%q): %v", dir, err)
	}
	if cfg.sandbox.readOnlyDir != canonDir {
		t.Fatalf("sandbox.readOnlyDir = %q, want %q: ReadOnlyDir must be re-locked explicitly", cfg.sandbox.readOnlyDir, canonDir)
	}

	name, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	if name != bwrapPath {
		t.Fatalf("wrapInvocation name = %q, want bwrap (no git-overlay sync shell expected)", name)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--bind "+canonDir+" "+canonDir) {
		t.Fatalf("ReadOnlyDir must not be bound read-write: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+canonDir+" "+canonDir) {
		t.Fatalf("ReadOnlyDir must be explicitly re-locked read-only: %s", joined)
	}
	// The re-lock must be the last bind entry — bwrap resolves overlapping
	// binds in argument order, so it must come after every writable root
	// (tmp/sandboxHome/sharedCache) to win when readOnlyDir sits inside one
	// of them, e.g. a manual-test rig staging everything under one /tmp
	// sandbox (the exact shape that shipped this bug).
	if args == nil {
		t.Fatalf("wrapInvocation returned nil args")
	}
	sep := slices.Index(args, "--")
	if sep < 3 || args[sep-3] != "--ro-bind" || args[sep-2] != canonDir || args[sep-1] != canonDir {
		t.Fatalf("ReadOnlyDir re-lock must be the last bind before the -- separator: %v", args)
	}
}

// TestWrapInvocation_Linux_ReadOnlyDirOverridesWritableRootContainingIt is
// the regression pin for the reported failure: a fallback dir nested inside
// a writable root (tmp in the manual-test repro) must still be denied writes
// because the ro-bind re-lock is appended after every writable --bind.
func TestWrapInvocation_Linux_ReadOnlyDirOverridesWritableRootContainingIt(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		sandboxHome: "/data/home",
		tmp:         "/tmp",
		sharedCache: "/data/cache",
		readOnlyDir: "/tmp/human-review-src",
	}}
	_, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	joined := strings.Join(args, " ")
	tmpIdx := strings.Index(joined, "--bind /tmp /tmp")
	roIdx := strings.Index(joined, "--ro-bind /tmp/human-review-src /tmp/human-review-src")
	if tmpIdx < 0 || roIdx < 0 {
		t.Fatalf("expected both the writable tmp bind and the read-only re-lock, got: %s", joined)
	}
	if roIdx < tmpIdx {
		t.Fatalf("read-only re-lock of a dir nested in tmp must be appended after the writable tmp bind: %s", joined)
	}
}

func TestWrapInvocation_Linux_NonEnforcePassThrough(t *testing.T) {
	for _, mode := range []string{"", "off", "report"} {
		cfg := &RunConfig{sandbox: sandboxSpec{mode: mode, worktree: "/data/wt"}}
		name, args := wrapInvocation("codex", []string{"exec"}, cfg)
		if name != "codex" || !slices.Equal(args, []string{"exec"}) {
			t.Errorf("mode %q must pass through unwrapped, got name=%s args=%v", mode, name, args)
		}
	}
	name, args := wrapInvocation("codex", []string{"exec"}, nil)
	if name != "codex" || !slices.Equal(args, []string{"exec"}) {
		t.Errorf("nil cfg must pass through unwrapped, got name=%s args=%v", name, args)
	}
}
