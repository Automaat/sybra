package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxEnforce_ZshHeredocSucceeds pins #3377. zsh writes every heredoc
// body to a temp file under $TMPPREFIX, and macOS's system zsh compiles that
// to /tmp/zsh and never re-derives it from TMPDIR. Both sandboxes grant the
// per-user temp root and not /tmp, so an enforce-mode agent lost heredocs
// entirely — the ordinary way an agent writes multi-line content.
//
// This runs the real dispatch path (prepareRunConfig → newProviderCmd →
// wrapInvocation) rather than a hand-built profile, because the defect was
// never in the profile: it was in which directory the shell reached for.
func TestSandboxEnforce_ZshHeredocSucceeds(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed; heredoc path unexercised on this host")
	}
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-heredoc",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	out := filepath.Join(worktree, "heredoc.txt")
	script := "cat > " + out + " << 'EOF'\nfirst line\nsecond line\nEOF\n"
	cmd := newProviderCmd(context.Background(), &cfg, false, zsh, "-c", script)
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	combined, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("heredoc under enforce failed: %v: %s", runErr, combined)
	}
	if strings.Contains(string(combined), "here document") {
		t.Fatalf("zsh refused the heredoc: %s", combined)
	}
	body, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("heredoc wrote no output: %v", readErr)
	}
	if got := string(body); got != "first line\nsecond line\n" {
		t.Fatalf("heredoc body = %q, want both lines", got)
	}
}

// TestSandboxEnforce_ZshHeredocTempStaysOutOfWorktree guards the obvious wrong
// fix. The worktree is writable under enforce, so pointing the shell's temp
// prefix there would also work — and then SanitizeWorktree's `git add -A`
// would commit whatever a heredoc left behind onto the branch.
func TestSandboxEnforce_ZshHeredocTempStaysOutOfWorktree(t *testing.T) {
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-heredoc-placement",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "report",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	prefix := ""
	for _, entry := range cfg.ExtraEnv {
		if value, ok := strings.CutPrefix(entry, shellTempPrefixEnv+"="); ok {
			prefix = value
		}
	}
	if prefix == "" {
		t.Fatalf("%s missing from run environment: %v", shellTempPrefixEnv, cfg.ExtraEnv)
	}
	rel, relErr := filepath.Rel(worktree, prefix)
	if relErr == nil && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("%s %q sits inside the worktree %q", shellTempPrefixEnv, prefix, worktree)
	}
}
