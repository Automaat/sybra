//go:build linux

package agent

import (
	"strings"
	"testing"
)

func TestWrapInvocation_Linux_ReadEnforceReplacesRootBind(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    "/data/wt",
		sandboxHome: "/data/home",
		tmp:         "/tmp",
		sharedCache: "/data/cache",
		readRoots:   []string{"/usr", "/etc", "/data/wt", "/home/sybra/.local/share/mise"},
	}}

	_, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	joined := strings.Join(args, " ")

	// The whole-filesystem bind is what makes every credential on the host
	// readable; read enforcement is meaningless if it survives.
	if strings.Contains(joined, "--ro-bind / /") {
		t.Fatalf("read enforcement left the whole filesystem bound: %s", joined)
	}
	for _, root := range []string{"/usr", "/etc", "/data/wt", "/home/sybra/.local/share/mise"} {
		if !strings.Contains(joined, "--ro-bind-try "+root+" "+root) {
			t.Errorf("read root %q not bound: %s", root, joined)
		}
	}
	// Writable roots are bound rw after the read binds, so they still win.
	if !strings.Contains(joined, "--bind /data/wt /data/wt") {
		t.Errorf("worktree lost read-write access under read enforcement: %s", joined)
	}
}

func TestWrapInvocation_Linux_ReadModeOffKeepsRootBind(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    "/data/wt",
		sandboxHome: "/data/home",
		tmp:         "/tmp",
		sharedCache: "/data/cache",
	}}

	_, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	joined := strings.Join(args, " ")

	// Empty readRoots is every deployment today; it must not silently start
	// restricting reads.
	if !strings.Contains(joined, "--ro-bind / /") {
		t.Fatalf("write-only enforce stopped binding the filesystem read-only: %s", joined)
	}
	if strings.Contains(joined, "--ro-bind-try") {
		t.Fatalf("read allowlist applied with no read roots configured: %s", joined)
	}
}
