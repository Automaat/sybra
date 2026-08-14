package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOneShotCommand_WrapsUnderEnforce pins that a classifier call is contained
// the way an agent spawn is. Asserting that some factory ran would pass against
// a bare exec.Command, which is the state #3383 described.
func TestOneShotCommand_WrapsUnderEnforce(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	m, _ := newTestManager(t, ManagerConfig{
		Runtime:     ManagerRuntimeConfig{SandboxMode: "enforce"},
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	dir := t.TempDir()

	cmd, cleanup, err := m.OneShotCommand(context.Background(), dir, "claude", []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("OneShotCommand: %v", err)
	}
	defer cleanup()

	if filepath.Base(cmd.Path) == "claude" {
		t.Fatalf("provider spawned unwrapped: %s %v", cmd.Path, cmd.Args)
	}
	if want := sandboxWrapperName(); filepath.Base(cmd.Path) != want {
		t.Fatalf("cmd.Path = %q, want the %s wrapper", cmd.Path, want)
	}
	if cmd.Dir != dir {
		t.Errorf("cmd.Dir = %q, want the directory it was given (%q)", cmd.Dir, dir)
	}
}

// TestOneShotCommand_WithholdsBoardAndToken pins that a call classifying
// attacker-influenced text is handed nothing to read out of its environment.
// Tools-off is a provider flag; this does not depend on a CLI honouring one.
func TestOneShotCommand_WithholdsBoardAndToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "operator-token")
	t.Setenv("GITHUB_TOKEN", "operator-token")
	t.Setenv("SYBRA_HOME", "/operator/board")
	m, _ := newTestManager(t, ManagerConfig{
		Runtime:     ManagerRuntimeConfig{SandboxMode: "report"},
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})

	cmd, cleanup, err := m.OneShotCommand(context.Background(), t.TempDir(), "claude", []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("OneShotCommand: %v", err)
	}
	defer cleanup()

	// exec resolves duplicate keys to the last value, so the injected blanks
	// win over the inherited environment.
	last := map[string]string{}
	for _, kv := range cmd.Env {
		if key, value, ok := strings.Cut(kv, "="); ok {
			last[key] = value
		}
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "SYBRA_AUTH_TOKEN", "SYBRA_AUTH_TOKEN_FILE"} {
		if last[key] != "" {
			t.Errorf("%s reached a one-shot call: %q", key, last[key])
		}
	}
	if last["SYBRA_HOME"] == "/operator/board" {
		t.Error("one-shot call inherited the operator board")
	}
	if last["SYBRA_HOME"] == "" {
		t.Error("one-shot call got no home at all; it must point at its own scratch")
	}
}

// TestOneShotCommand_CleanupRemovesScratchHome pins that a long-lived server
// does not accumulate one sandbox home per classification. Removal is the
// caller's to trigger, since the context these calls carry is the app's root
// and is done only at shutdown.
func TestOneShotCommand_CleanupRemovesScratchHome(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		Runtime:     ManagerRuntimeConfig{SandboxMode: "report"},
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})

	cmd, cleanup, err := m.OneShotCommand(context.Background(), t.TempDir(), "claude", []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("OneShotCommand: %v", err)
	}
	home := ""
	for _, kv := range cmd.Env {
		if value, ok := strings.CutPrefix(kv, "SYBRA_HOME="); ok {
			home = value
		}
	}
	if home == "" {
		t.Fatal("no scratch home in the call environment")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("scratch home missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("scratch home survived cleanup: %v", err)
	}
}

// TestOneShotCommand_ResolvesSymlinkedProvider pins that a one-shot call gets
// the same launcher resolution an agent spawn does. Homebrew installs codex as
// a symlink whose mandatory sibling helper exists only beside the real target,
// so an unresolved argv[0] fails under enforce.
func TestOneShotCommand_ResolvesSymlinkedProvider(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	binDir := t.TempDir()
	target := filepath.Join(binDir, "codex-real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write provider: %v", err)
	}
	link := filepath.Join(binDir, "codex")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink provider: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m, _ := newTestManager(t, ManagerConfig{
		Runtime:     ManagerRuntimeConfig{SandboxMode: "enforce"},
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	cmd, cleanup, err := m.OneShotCommand(context.Background(), t.TempDir(), "codex", []string{"exec"})
	if err != nil {
		t.Fatalf("OneShotCommand: %v", err)
	}
	defer cleanup()

	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, target) {
		t.Fatalf("provider launched through the symlink rather than its target:\n%s", joined)
	}
}
