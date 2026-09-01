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

// newAmbientMiseStore points this process at a fixture mise store holding one
// installed tool version and one plugin, and returns the store path.
func newAmbientMiseStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, ".local", "share", "mise")
	for _, dir := range []string{
		filepath.Join(store, "installs", "go", "1.26.6", "bin"),
		filepath.Join(store, "plugins", "lua"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("MISE_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	return store
}

func TestInjectMiseDataDir_KeepsTheShimRebuildInsideTheSandbox(t *testing.T) {
	// Given an enforced run and an operator store holding the pinned tools
	store := newAmbientMiseStore(t)
	cfg := RunConfig{TaskID: "t1", SandboxMode: "enforce", resolvedSandboxHome: t.TempDir()}

	// When the mise data dir is injected
	if err := (&Manager{logger: discardLogger()}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the run writes into the sandbox while reading the operator's tools
	dataDir := miseDataDirValue(cfg.ExtraEnv)
	if dataDir != filepath.Join(cfg.resolvedSandboxHome, "mise") {
		t.Fatalf("MISE_DATA_DIR = %q, want it under the sandbox home", dataDir)
	}
	linked, err := os.Readlink(filepath.Join(dataDir, "installs", "go", "1.26.6"))
	if err != nil {
		t.Fatalf("installed version is not linked: %v", err)
	}
	if linked != filepath.Join(store, "installs", "go", "1.26.6") {
		t.Fatalf("version links to %q, want the operator store", linked)
	}
	if _, err := os.Readlink(filepath.Join(dataDir, "plugins", "lua")); err != nil {
		t.Fatalf("plugin is not linked, so mise would re-clone it: %v", err)
	}
}

func TestInjectMiseDataDir_LeavesRoomForAVersionTheOperatorLacks(t *testing.T) {
	// Given an enforced run against a store with one Go version installed
	newAmbientMiseStore(t)
	cfg := RunConfig{TaskID: "t1", SandboxMode: "enforce", resolvedSandboxHome: t.TempDir()}
	if err := (&Manager{logger: discardLogger()}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}
	dataDir := miseDataDirValue(cfg.ExtraEnv)

	// When a toolchain bump installs a version the operator does not have
	fresh := filepath.Join(dataDir, "installs", "go", "1.27.0")
	err := os.MkdirAll(fresh, 0o755)

	// Then it lands in the sandbox rather than failing on the read-only store
	if err != nil {
		t.Fatalf("a new version could not be installed into the sandbox: %v", err)
	}
	info, statErr := os.Lstat(filepath.Join(dataDir, "installs", "go"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the tool directory is a link, so a new version would be written into the operator store")
	}
}

func TestInjectMiseDataDir_ReplacesAStaleLink(t *testing.T) {
	// Given a durable sandbox home carrying a link to a store that moved
	store := newAmbientMiseStore(t)
	sandboxHome := t.TempDir()
	stale := filepath.Join(sandboxHome, "mise", "installs", "go")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(stale, "1.26.6")); err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{TaskID: "t1", SandboxMode: "enforce", resolvedSandboxHome: sandboxHome}

	// When the mise data dir is injected again
	if err := (&Manager{logger: discardLogger()}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the link names the current store instead of the vanished one
	linked, err := os.Readlink(filepath.Join(stale, "1.26.6"))
	if err != nil {
		t.Fatalf("link missing: %v", err)
	}
	if linked != filepath.Join(store, "installs", "go", "1.26.6") {
		t.Fatalf("stale link survived: %q", linked)
	}
}

func TestInjectMiseDataDir_DropsACallerSuppliedStore(t *testing.T) {
	// Given a run whose caller supplied its own mise store
	newAmbientMiseStore(t)
	attacker := t.TempDir()
	if err := os.MkdirAll(filepath.Join(attacker, "installs", "go", "1.26.6"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := RunConfig{
		TaskID: "t1", SandboxMode: "enforce", resolvedSandboxHome: t.TempDir(),
		ExtraEnv: []string{"MISE_DATA_DIR=" + attacker},
	}

	// When the mise data dir is injected
	if err := (&Manager{logger: discardLogger()}).injectMiseDataDir(&cfg); err != nil {
		t.Fatalf("injectMiseDataDir: %v", err)
	}

	// Then the caller's store never reaches the process environment
	if slices.Contains(cfg.ExtraEnv, "MISE_DATA_DIR="+attacker) {
		t.Fatal("caller-supplied mise store survived into the environment")
	}
	if got := miseDataDirValue(cfg.ExtraEnv); got != filepath.Join(cfg.resolvedSandboxHome, "mise") {
		t.Fatalf("MISE_DATA_DIR = %q, want the sandbox mirror", got)
	}
}

func TestInjectMiseDataDir_NamesTheHostStoreWhenItCannotMirror(t *testing.T) {
	tests := []struct {
		name string
		cfg  RunConfig
	}{
		{"no sandbox home", RunConfig{TaskID: "t1", SandboxMode: "enforce", ExtraEnv: []string{"MISE_DATA_DIR=.local/share/mise"}}},
		{"posture is not enforce", RunConfig{TaskID: "t1", SandboxMode: "report", resolvedSandboxHome: t.TempDir(), ExtraEnv: []string{"MISE_DATA_DIR=.local/share/mise"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a run the mirror does not apply to
			store := newAmbientMiseStore(t)
			cfg := tc.cfg

			// When the mise data dir is injected
			if err := (&Manager{logger: discardLogger()}).injectMiseDataDir(&cfg); err != nil {
				t.Fatalf("injectMiseDataDir: %v", err)
			}

			// Then the relative value is replaced by the absolute host store
			got := miseDataDirValue(cfg.ExtraEnv)
			if got != store {
				t.Fatalf("MISE_DATA_DIR = %q, want the host store %q", got, store)
			}
		})
	}
}

func TestPrepareRunConfig_MirrorsMiseForEveryRole(t *testing.T) {
	for _, role := range []Role{RoleImplementation, RoleTestRunner, RoleFixReview} {
		t.Run(string(role), func(t *testing.T) {
			// Given an enforced run of this role
			newAmbientMiseStore(t)
			t.Setenv("SYBRA_HOME", t.TempDir())
			sandboxDir := t.TempDir()
			m, _ := newTestManager(t, ManagerConfig{
				SandboxHome: func(string) (string, error) { return sandboxDir, nil },
			})

			// When its run config is prepared
			cfg, _, err := m.prepareRunConfig(RunConfig{
				TaskID: "task-1", Mode: "headless", Role: role, SandboxMode: "enforce", Dir: t.TempDir(),
			})
			if err != nil {
				t.Fatalf("prepareRunConfig: %v", err)
			}

			// Then mise writes inside the sandbox, by an absolute path
			got := miseDataDirValue(cfg.ExtraEnv)
			if got != filepath.Join(sandboxDir, "mise") {
				t.Fatalf("MISE_DATA_DIR = %q, want %q", got, filepath.Join(sandboxDir, "mise"))
			}
		})
	}
}
