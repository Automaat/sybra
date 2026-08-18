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
	narrowSandboxTempRoot(t)
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
	if strings.HasPrefix(cfg.sandbox.tmp, "/tmp") {
		t.Skipf("granted temp root %q still covers zsh's default prefix; the run cannot distinguish the fix", cfg.sandbox.tmp)
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

	// The obvious alternative fix is to grant /tmp instead, which widens containment for every run. Prove the same config still refuses it.
	denied := filepath.Join("/tmp", "sybra-heredoc-denied-probe")
	t.Cleanup(func() { _ = os.Remove(denied) })
	probe := newProviderCmd(context.Background(), &cfg, false, zsh, "-c", "echo leak > "+denied)
	probe.Env = append(os.Environ(), cfg.ExtraEnv...)
	if probeOut, probeErr := probe.CombinedOutput(); probeErr == nil {
		t.Fatalf("write to %q succeeded under enforce: %s", denied, probeOut)
	}
	if _, statErr := os.Stat(denied); !os.IsNotExist(statErr) {
		t.Fatalf("unrelated temp path %q became writable: %v", denied, statErr)
	}
}

// TestSandboxEnforce_ZshHeredocSurvivesScratchCleanup guards the other wrong
// fix. The run's prompt advertises $SYBRA_SCRATCH_HOME as disposable, so an
// agent that deletes it must not lose heredocs for the rest of the run.
func TestSandboxEnforce_ZshHeredocSurvivesScratchCleanup(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed; heredoc path unexercised on this host")
	}
	narrowSandboxTempRoot(t)
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-heredoc-cleanup",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if strings.HasPrefix(cfg.sandbox.tmp, "/tmp") {
		t.Skipf("granted temp root %q still covers zsh's default prefix; the run cannot distinguish the fix", cfg.sandbox.tmp)
	}

	out := filepath.Join(worktree, "after-cleanup.txt")
	script := "rm -rf \"$SYBRA_SCRATCH_HOME\"\ncat > " + out + " << 'EOF'\nstill working\nEOF\n"
	cmd := newProviderCmd(context.Background(), &cfg, false, zsh, "-c", script)
	cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	combined, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("heredoc after scratch cleanup failed: %v: %s", runErr, combined)
	}
	body, readErr := os.ReadFile(out)
	if readErr != nil || string(body) != "still working\n" {
		t.Fatalf("heredoc body = %q (err %v), want the written line", body, readErr)
	}
}

// TestShellTempPrefix_UnavailableDirectoryDropsCallerValue pins what happens
// when the prefix directory cannot be created. The run still dispatches, and
// a caller-supplied prefix — which can name any writable path, the worktree
// included — is dropped rather than left to steer the shell's temp files.
func TestShellTempPrefix_UnavailableDirectoryDropsCallerValue(t *testing.T) {
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sandboxHome, "zsh"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("stage blocking file: %v", err)
	}
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-heredoc-blocked",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "report",
		ExtraEnv:    []string{shellTempPrefixEnv + "=" + filepath.Join(worktree, "evil")},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig refused to dispatch: %v", err)
	}
	for _, entry := range cfg.ExtraEnv {
		if strings.HasPrefix(entry, shellTempPrefixEnv+"=") {
			t.Fatalf("caller %s survived an unusable prefix directory: %q", shellTempPrefixEnv, entry)
		}
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

// TestSandboxEnforce_ScratchDirIsWritableAndTmpIsNot pins the pair the prompt
// tells agents to rely on: the advertised scratch directory accepts an
// ordinary file, and the /tmp path agents reach for instead does not.
func TestSandboxEnforce_ScratchDirIsWritableAndTmpIsNot(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	narrowSandboxTempRoot(t)
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-scratch-dir",
		Mode:        "headless",
		Dir:         worktree,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if strings.HasPrefix(cfg.sandbox.tmp, "/tmp") {
		t.Skipf("granted temp root %q covers /tmp; the run cannot distinguish the two paths", cfg.sandbox.tmp)
	}

	write := newProviderCmd(context.Background(), &cfg, false, sh, "-c", `echo scratch > "$SYBRA_SCRATCH_DIR/probe.txt"`)
	write.Env = append(os.Environ(), cfg.ExtraEnv...)
	if out, writeErr := write.CombinedOutput(); writeErr != nil {
		t.Fatalf("write to $SYBRA_SCRATCH_DIR failed under enforce: %v: %s", writeErr, out)
	}
	body, readErr := os.ReadFile(filepath.Join(sandboxHome, "scratch", "probe.txt"))
	if readErr != nil || string(body) != "scratch\n" {
		t.Fatalf("scratch file = %q (err %v), want the written line", body, readErr)
	}

	denied := "/tmp/sybra-scratch-guidance-probe.txt"
	t.Cleanup(func() { _ = os.Remove(denied) })
	probe := newProviderCmd(context.Background(), &cfg, false, sh, "-c", "echo leak > "+denied)
	probe.Env = append(os.Environ(), cfg.ExtraEnv...)
	if out, probeErr := probe.CombinedOutput(); probeErr == nil {
		t.Fatalf("write to %q succeeded, so the prompt's rule about /tmp is wrong: %s", denied, out)
	}
}
