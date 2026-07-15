//go:build linux

package agent

import (
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
	if !strings.Contains(joined, "--ro-bind / / --dev /dev --proc /proc") {
		t.Fatalf("expected read-only root + fresh dev/proc bwrap args, got: %s", joined)
	}
	for _, root := range []string{
		"/data/wt",
		"/data/home", "/tmp", "/data/cache",
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
	sep := slices.Index(args, "--")
	if sep < 0 || sep+1 >= len(args) || args[sep+1] != "claude" {
		t.Fatalf("provider must follow the -- separator, got: %s", joined)
	}
	if !slices.Equal(args[sep+1:], []string{"claude", "-p", "hi"}) {
		t.Errorf("provider argv altered: %v", args[sep+1:])
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
