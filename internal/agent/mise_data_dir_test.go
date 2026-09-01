package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func miseDataDirValue(env []string) string {
	value := ""
	for _, assignment := range env {
		if configured, ok := strings.CutPrefix(assignment, "MISE_DATA_DIR="); ok {
			value = configured
		}
	}
	return value
}

func newAmbientMiseStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	installs := filepath.Join(home, ".local", "share", "mise", "installs", "go", "1.26.6")
	if err := os.MkdirAll(installs, 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestInjectMiseDataDir_KeepsTheShimRebuildInsideTheSandbox(t *testing.T) {
	// Given a sandboxed run and an operator mise store holding the pinned tools
	home := newAmbientMiseStore(t)
	cfg := RunConfig{TaskID: "t1", resolvedSandboxHome: t.TempDir(), ExtraEnv: []string{"HOME=" + home}}

	// When the mise data dir is injected
	if err := (&Manager{}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the run writes into the sandbox while still reading the real installs
	dataDir := miseDataDirValue(cfg.ExtraEnv)
	if dataDir != filepath.Join(cfg.resolvedSandboxHome, "mise") {
		t.Fatalf("MISE_DATA_DIR = %q, want it under the sandbox home", dataDir)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(dataDir, "installs"))
	if err != nil {
		t.Fatalf("installs link: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "share", "mise", "installs"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("installs link resolves to %q, want the operator store %q", resolved, want)
	}
	if _, err := os.Stat(filepath.Join(resolved, "go", "1.26.6")); err != nil {
		t.Fatalf("pinned tool not visible through the link: %v", err)
	}
}

func TestInjectMiseDataDir_OverridesAnAmbientDataDir(t *testing.T) {
	// Given a run that already carries the operator's data dir
	home := newAmbientMiseStore(t)
	ambient := filepath.Join(home, ".local", "share", "mise")
	cfg := RunConfig{TaskID: "t1", resolvedSandboxHome: t.TempDir(), ExtraEnv: []string{"MISE_DATA_DIR=" + ambient}}

	// When the mise data dir is injected
	if err := (&Manager{}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the sandbox copy replaces it rather than being shadowed by it
	if got := miseDataDirValue(cfg.ExtraEnv); got == ambient {
		t.Fatal("MISE_DATA_DIR still points at the operator store")
	}
	if slices.Contains(cfg.ExtraEnv, "MISE_DATA_DIR="+ambient) {
		t.Fatal("the operator store assignment survived in the environment")
	}
}

func TestInjectMiseDataDir_LeavesAnUnsandboxedRunAlone(t *testing.T) {
	// Given a run with no sandbox home
	home := newAmbientMiseStore(t)
	cfg := RunConfig{TaskID: "t1", ExtraEnv: []string{"HOME=" + home}}

	// When the mise data dir is injected
	if err := (&Manager{}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then nothing is redirected
	if got := miseDataDirValue(cfg.ExtraEnv); got != "" {
		t.Fatalf("MISE_DATA_DIR = %q, want it untouched", got)
	}
}

func TestInjectMiseDataDir_SkipsAHostWithNoStore(t *testing.T) {
	// Given a host whose mise store does not exist
	cfg := RunConfig{TaskID: "t1", resolvedSandboxHome: t.TempDir(), ExtraEnv: []string{"HOME=" + t.TempDir()}}

	// When the mise data dir is injected
	if err := (&Manager{}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the run is left as it was, with no empty store to hide the tools
	if got := miseDataDirValue(cfg.ExtraEnv); got != "" {
		t.Fatalf("MISE_DATA_DIR = %q, want no redirect without an installs tree", got)
	}
}
