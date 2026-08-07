package agent

import (
	"bytes"
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
			panic("unreachable")
		}
	}

	sandboxHome := t.TempDir()
	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	wantClaude, err := canonicalizeRoot(claude)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if spec.claudeState != wantClaude {
		t.Errorf("claudeState = %q, want %q (the CLI must be able to persist session state)", spec.claudeState, wantClaude)
	}
	wantCache, err := canonicalizeRoot(cache)
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if spec.toolCache != wantCache {
		t.Errorf("toolCache = %q, want %q (mise must be able to write its cache)", spec.toolCache, wantCache)
	}
}

func TestEnforceSpec_FallsBackWhenStateDirAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sandboxHome := t.TempDir()

	spec := enforceSpec("/wt", nil, sandboxHome, "/tmp", "", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

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
		panic("unreachable")
	}
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := enforceSpec("/wt", nil, t.TempDir(), "/tmp", "", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

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

// TestEnforceSpec_MaterializesAbsentDurableConfig closes the bypass that
// skipping absent paths would leave: the enclosing state dir is writable, so
// a run could create settings.json itself and persist hooks that way — the
// exact channel this protection exists to remove. The paths must exist so
// they can be denied.
func TestEnforceSpec_MaterializesAbsentDurableConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	spec := enforceSpec("/wt", nil, t.TempDir(), "/tmp", "", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	for _, want := range []string{"settings.json", "hooks"} {
		found := false
		for _, got := range spec.stateDenied {
			if strings.HasSuffix(got, want) {
				found = true
				if _, err := os.Stat(got); err != nil {
					t.Errorf("%s is denied but does not exist (%v); the bind would fail the spawn", want, err)
				}
			}
		}
		if !found {
			t.Errorf("%s missing from stateDenied %v — a run could create it and persist hooks", want, spec.stateDenied)
		}
	}
}

// TestEnforceSpec_KeepsExistingDurableConfig guards against the protection
// clobbering real operator config on the way to protecting it.
func TestEnforceSpec_KeepsExistingDurableConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	existing := []byte(`{"hooks":{"PreToolUse":[]}}`)
	if err := os.WriteFile(filepath.Join(claude, "settings.json"), existing, 0o644); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}

	enforceSpec("/wt", nil, t.TempDir(), "/tmp", "", "/cache", "/profile", "", gitSandboxRoots{}, gitSandboxOverlay{})

	got, err := os.ReadFile(filepath.Join(claude, "settings.json"))
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if !bytes.Equal(got, existing) {
		t.Fatalf("settings.json = %q, want it left untouched", got)
	}
}
