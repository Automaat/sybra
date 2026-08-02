package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnforceSpec_ResolvesAgentStateRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// projects/ only: the state dir root stays read-only so settings.json
	// hooks cannot persist into later runs, while --resume keeps the
	// transcript it reads from here (#2779).
	claude := filepath.Join(home, ".claude", "projects")
	cache := filepath.Join(home, ".cache")
	for _, d := range []string{claude, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sandboxHome := t.TempDir()
	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	wantClaude, err := canonicalizeRoot(claude)
	if err != nil {
		t.Fatal(err)
	}
	if spec.claudeState != wantClaude {
		t.Errorf("claudeState = %q, want %q (--resume reads its transcript from projects/)", spec.claudeState, wantClaude)
	}
	wantCache, err := canonicalizeRoot(cache)
	if err != nil {
		t.Fatal(err)
	}
	if spec.toolCache != wantCache {
		t.Errorf("toolCache = %q, want %q (mise must be able to write its cache)", spec.toolCache, wantCache)
	}
}

func TestEnforceSpec_FallsBackWhenStateDirAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sandboxHome := t.TempDir()

	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	for name, got := range map[string]string{
		"codexState":    spec.codexState,
		"copilotState":  spec.copilotState,
		"opencodeState": spec.opencodeState,
	} {
		if got != sandboxHome {
			t.Errorf("%s = %q, want fallback to sandboxHome %q — an absent state dir must not produce an empty write root", name, got, sandboxHome)
		}
	}
}

// TestEnforceSpec_ClaudeStateExcludesSettings pins the point of narrowing the
// claude write root: settings.json must fall outside it. Its PreToolUse hooks
// execute in every later run — including verifier roles — and survive worktree
// cleanup, so a writable state root is a persistence channel, not just a
// shared directory.
func TestEnforceSpec_ClaudeStateExcludesSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sandboxHome := t.TempDir()

	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	settings, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	settings = filepath.Join(settings, ".claude", "settings.json")
	if strings.HasPrefix(settings, spec.claudeState+string(filepath.Separator)) {
		t.Fatalf("settings.json (%s) lies inside the writable root %s", settings, spec.claudeState)
	}
	if !strings.HasSuffix(spec.claudeState, filepath.Join(".claude", "projects")) {
		t.Fatalf("claudeState = %q, want it scoped to .claude/projects so --resume still works", spec.claudeState)
	}
}
