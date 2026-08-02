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

	claude := filepath.Join(home, ".claude")
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

// TestEnforceSpec_DeniesDurableClaudeConfig pins the shape of the protection.
// The state dir must stay writable — a real multi-turn run writes plugins/,
// sessions/, session-env/, shell-snapshots/ and projects/, and narrowing to an
// allowlist broke runs outright — so the files that decide how *later* runs
// behave are carved back out of it instead.
func TestEnforceSpec_DeniesDurableClaudeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(claude, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := enforceSpec("/wt", nil, t.TempDir(), "/tmp", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	if !strings.HasSuffix(spec.claudeState, ".claude") {
		t.Fatalf("claudeState = %q, want the whole state dir writable", spec.claudeState)
	}
	for _, want := range []string{"settings.json", "hooks"} {
		found := false
		for _, got := range spec.stateDenied {
			if strings.HasSuffix(got, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from stateDenied %v — a run could change how later runs behave", want, spec.stateDenied)
		}
	}
}

// TestEnforceSpec_SkipsAbsentDurableConfig guards the spawn: binding a path
// that does not exist fails the run, and a missing settings.json is nothing to
// protect in the first place.
func TestEnforceSpec_SkipsAbsentDurableConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := enforceSpec("/wt", nil, t.TempDir(), "/tmp", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	if len(spec.stateDenied) != 0 {
		t.Fatalf("stateDenied = %v, want empty when no durable config exists", spec.stateDenied)
	}
}
