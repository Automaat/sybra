//go:build darwin || linux

package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkflowGitCancellationKillsHelperBeforeWorktreeReuse(t *testing.T) {
	bin := t.TempDir()
	gitPath := filepath.Join(bin, "git")
	script := `#!/bin/sh
set -eu
case "$1" in
  hold)
    /bin/sleep 30 &
    child=$!
    printf '%s\n' "$child" > "$WORKFLOW_GIT_HELPER_PID"
    wait "$child"
    ;;
  probe)
    child="$(/bin/cat "$WORKFLOW_GIT_HELPER_PID")"
    if kill -0 "$child" 2>/dev/null; then
      printf 'helper still retains the worktree\n' >&2
      exit 73
    fi
    ;;
esac
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	t.Setenv("WORKFLOW_GIT_HELPER_PID", pidFile)
	wtPath := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- gitDo(ctx, wtPath, "hold")
	}()

	waitForWorkflowGitHelperPID(t, pidFile)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("gitDo unexpectedly succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workflow Git command did not stop after cancellation")
	}

	if !pollUntil(3*time.Second, 20*time.Millisecond, func() bool {
		return gitOK(context.Background(), wtPath, "probe")
	}) {
		t.Fatal("Git helper survived cancellation and retained the worktree")
	}
}

func waitForWorkflowGitHelperPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	ok := pollUntil(3*time.Second, 10*time.Millisecond, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		_, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
		return scanErr == nil
	})
	if !ok {
		t.Fatalf("timed out waiting for Git helper PID in %s", path)
	}
	return pid
}
