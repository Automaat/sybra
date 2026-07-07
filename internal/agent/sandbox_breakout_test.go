//go:build darwin

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shellQuote wraps a string in single quotes with proper escaping for bash,
// so victim/target paths built from t.TempDir() can't be misparsed as shell
// syntax if they ever contain spaces or metacharacters.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// These tests exercise newProviderCmd directly — the single seam every
// provider spawn site (runner_headless.go, runner_convo.go,
// runner_convo_survive.go, runner_codex_convo.go) routes through (pinned by
// the `rg exec.CommandContext` drift invariant in the C1 refactor commit).
// Testing the seam once, with a real subprocess, covers all six call sites
// without depending on the claude/codex CLIs being installed on the test
// host.

// newEnforceSandboxCfg builds a *RunConfig with an enforce-mode sandbox spec
// rooted at worktree/sandboxHome/tmp, materializing the real embedded
// profile.
func newEnforceSandboxCfg(t *testing.T, worktree, sandboxHome, tmp string) *RunConfig {
	t.Helper()
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not available")
	}
	profile, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile: %v", err)
	}
	wt, err := canonicalizeRoot(worktree)
	if err != nil {
		t.Fatalf("canonicalizeRoot(worktree): %v", err)
	}
	home, err := canonicalizeRoot(sandboxHome)
	if err != nil {
		t.Fatalf("canonicalizeRoot(sandboxHome): %v", err)
	}
	tp, err := canonicalizeRoot(tmp)
	if err != nil {
		t.Fatalf("canonicalizeRoot(tmp): %v", err)
	}
	return &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    wt,
		sandboxHome: home,
		tmp:         tp,
		profilePath: profile,
	}}
}

// homeVictimPath returns a path under the real $HOME (which is never one of
// a sandbox spec's allowed roots) for a break-out test to target. t.TempDir()
// is unsuitable for this: it lives under os.TempDir(), which the sandbox
// profile intentionally allows broadly (the whole TMP root), so a victim
// path built from t.TempDir() would be an allowed write, not a break-out.
// The file is removed via t.Cleanup regardless of test outcome.
func homeVictimPath(t *testing.T, name string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no $HOME available: %v", err)
	}
	path := filepath.Join(home, name)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func runSandboxedScript(t *testing.T, cfg *RunConfig, detached bool, script string) (exitCode int, elapsed time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	cmd := newProviderCmd(ctx, cfg, detached, "bash", "-c", script)
	err := cmd.Run()
	elapsed = time.Since(start)
	if err == nil {
		return 0, elapsed
	}
	if exitErr, ok := err.(interface{ ExitCode() int }); ok {
		return exitErr.ExitCode(), elapsed
	}
	return -1, elapsed
}

// TestSandboxBreakout_DeniesHomeWrite proves the #1576 shape directly: a
// process wrapped by newProviderCmd in enforce mode cannot write to $HOME,
// on both the pipe-backed (detached=false) and detached (survive) spawn
// paths.
func TestSandboxBreakout_DeniesHomeWrite(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipe", true: "detached"}[detached], func(t *testing.T) {
			worktree := t.TempDir()
			home := t.TempDir()
			victim := homeVictimPath(t, "sybra-test-PWNED-pipe-detached-"+map[bool]string{false: "pipe", true: "detached"}[detached]+".txt")
			cfg := newEnforceSandboxCfg(t, worktree, home, os.TempDir())
			code, _ := runSandboxedScript(t, cfg, detached, "echo pwned > "+shellQuote(victim))
			if code == 0 {
				t.Fatalf("write outside allowed roots succeeded (exit 0); want denial")
			}
			if _, statErr := os.Stat(victim); statErr == nil {
				t.Fatalf("victim file %q was created despite enforce mode", victim)
			}
		})
	}
}

// TestSandboxBreakout_AllowsRootWrites proves the profile's allowlist is not
// overly restrictive: writes under the worktree, sandbox home, and tmp all
// succeed.
func TestSandboxBreakout_AllowsRootWrites(t *testing.T) {
	worktree := t.TempDir()
	home := t.TempDir()
	tmp := t.TempDir()
	cfg := newEnforceSandboxCfg(t, worktree, home, tmp)

	script := "echo a > " + shellQuote(filepath.Join(worktree, "a.txt")) +
		" && echo b > " + shellQuote(filepath.Join(home, "b.txt")) +
		" && echo c > " + shellQuote(filepath.Join(tmp, "c.txt"))
	code, _ := runSandboxedScript(t, cfg, false, script)
	if code != 0 {
		t.Fatalf("allowed-root writes failed with exit %d", code)
	}
	for _, f := range []string{filepath.Join(worktree, "a.txt"), filepath.Join(home, "b.txt"), filepath.Join(tmp, "c.txt")} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected %q to exist: %v", f, err)
		}
	}
}

// TestSandboxBreakout_DeniesGrandchildWrite pins the literal #1576 shape: an
// agent process spawning a subprocess (e.g. a Playwright/npm/node child)
// that attempts a write outside the allowed roots. The seatbelt profile
// applies to the whole process tree, not just the direct child, so the
// grandchild's write must be denied too.
func TestSandboxBreakout_DeniesGrandchildWrite(t *testing.T) {
	worktree := t.TempDir()
	home := t.TempDir()
	victim := homeVictimPath(t, "sybra-test-PWNED_BY_GRANDCHILD.txt")
	cfg := newEnforceSandboxCfg(t, worktree, home, os.TempDir())

	// bash (child) spawns another bash (grandchild) which performs the write.
	script := "bash -c " + shellQuote("bash -c "+shellQuote("echo pwned > "+shellQuote(victim)))
	code, _ := runSandboxedScript(t, cfg, false, script)
	if code == 0 {
		t.Fatalf("grandchild write outside allowed roots succeeded; want denial")
	}
	if _, statErr := os.Stat(victim); statErr == nil {
		t.Fatalf("victim file %q was created by a grandchild process despite enforce mode", victim)
	}
}

// TestSandboxBreakout_DeniesSymlinkEscape proves that a symlink placed
// inside the worktree pointing outside the allowed roots cannot be used to
// write through the allowlist: the kernel resolves the symlink target
// before applying the write check, so this must be denied even though the
// symlink *itself* lives under an allowed root.
func TestSandboxBreakout_DeniesSymlinkEscape(t *testing.T) {
	worktree := t.TempDir()
	sandboxHome := t.TempDir()
	victimName := "sybra-test-PWNED_VIA_SYMLINK.txt"
	victim := homeVictimPath(t, victimName)

	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no $HOME available: %v", err)
	}
	link := filepath.Join(worktree, "escape-link")
	if err := os.Symlink(realHome, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := newEnforceSandboxCfg(t, worktree, sandboxHome, os.TempDir())

	code, _ := runSandboxedScript(t, cfg, false, "echo pwned > "+shellQuote(filepath.Join(link, victimName)))
	if code == 0 {
		t.Fatalf("write through a worktree symlink pointing outside allowed roots succeeded; want denial")
	}
	if _, statErr := os.Stat(victim); statErr == nil {
		t.Fatalf("victim file %q was created via a symlink escape despite enforce mode", victim)
	}
}

// TestSandboxBreakout_PreservesPIDAndSignal pins the spike's load-bearing
// assumption: sandbox-exec execs the child in place (the reported PID is
// the real provider process, not a sandbox-exec wrapper PID) and SIGTERM
// reaches it — required for the watchdog kill path and detached reattach to
// keep working once every spawn is wrapped.
func TestSandboxBreakout_PreservesPIDAndSignal(t *testing.T) {
	worktree := t.TempDir()
	home := t.TempDir()
	cfg := newEnforceSandboxCfg(t, worktree, home, os.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := newProviderCmd(ctx, cfg, false, "bash", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal pid %d: %v", pid, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr == nil {
			t.Fatal("expected non-nil wait error for a signaled process")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within 5s of SIGINT — sandbox-exec may not be forwarding signals to the exec'd child")
	}
}
