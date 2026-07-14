//go:build linux

package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestWrapInvocation_Linux_EnforceBindsOnlyWriteRoots(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:          "enforce",
		worktree:      "/data/wt",
		gitMetadata:   []string{"/data/clones/repo.git/worktrees/task", "/data/clones/repo.git"},
		gitShared:     []string{"/data/clones/repo.git/objects", "/data/clones/repo.git/refs/remotes", "/data/clones/repo.git/refs/heads/fix"},
		gitReadonly:   []string{"/data/clones/repo.git/refs/heads/fix/sibling"},
		sandboxHome:   "/data/home",
		tmp:           "/tmp",
		sharedCache:   "/data/cache",
		gitAdminDir:   "/data/clones/repo.git/worktrees/task",
		gitCommonDir:  "/data/clones/repo.git",
		gitWorktrees:  "/data/clones/repo.git/worktrees",
		claudeState:   "/data/sybra/claude",
		codexState:    "/data/sybra/codex",
		copilotState:  "/data/sybra/copilot",
		opencodeState: "/data/sybra/opencode",
		toolCache:     "/home/sybra/.cache",
	}}
	_, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)

	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "--ro-bind / / --dev /dev --proc /proc") {
		t.Fatalf("expected read-only root + fresh dev/proc prefix, got: %s", joined)
	}
	for _, root := range []string{
		"/data/wt", "/data/clones/repo.git/objects", "/data/clones/repo.git/refs/remotes", "/data/clones/repo.git/refs/heads/fix",
		"/data/home", "/tmp", "/data/cache",
		"/data/sybra/claude", "/data/sybra/codex", "/data/sybra/copilot", "/data/sybra/opencode", "/home/sybra/.cache",
	} {
		if !strings.Contains(joined, "--bind "+root+" "+root) {
			t.Errorf("write root %q not bound read-write: %s", root, joined)
		}
	}
	if strings.Contains(joined, "--bind /data/clones/repo.git /data/clones/repo.git") {
		t.Fatalf("shared git common dir must not be reopened read-write: %s", joined)
	}
	if !strings.Contains(joined, "--bind /data/clones/repo.git/worktrees/task /data/clones/repo.git/worktrees/task") {
		t.Fatalf("task git admin dir must be reopened read-write: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind /data/clones/repo.git/worktrees /data/clones/repo.git/worktrees") {
		t.Fatalf("shared worktrees dir must remain read-only: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind /data/clones/repo.git/refs/heads/fix/sibling /data/clones/repo.git/refs/heads/fix/sibling") {
		t.Fatalf("sibling branch ref must be re-bound read-only: %s", joined)
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

func TestDedupeRoots_DropsEmptyAndDuplicate(t *testing.T) {
	got := dedupeRoots("/a", "/a", "", "/b", "  ", "/a")
	if !slices.Equal(got, []string{"/a", "/b"}) {
		t.Errorf("dedupeRoots = %v, want [/a /b]", got)
	}
}
