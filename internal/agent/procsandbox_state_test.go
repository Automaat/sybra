package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnforceSpec_ResolvesAgentStateRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	claude := filepath.Join(home, ".claude")
	cache := filepath.Join(home, ".cache")
	for _, d := range []string{claude, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	sandboxHome := t.TempDir()
	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "/cache", "/profile", gitSandboxRoots{})

	wantClaude, err := canonicalizeRoot(claude)
	if err != nil {
		t.Fatal(err)
	}
	if spec.claudeState != wantClaude {
		t.Errorf("claudeState = %q, want %q (the CLI must be able to persist session state)", spec.claudeState, wantClaude)
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

	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "/cache", "/profile", gitSandboxRoots{})

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
