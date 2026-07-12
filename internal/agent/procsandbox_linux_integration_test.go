//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
