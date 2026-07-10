package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareRunConfig_SandboxHome_Injected pins the core #1576 fix: a
// task-scoped run gets SYBRA_HOME pointed at the sandbox, appended last so it
// wins over anything already in cfg.ExtraEnv, plus SYBRA_CONTROL_HOME when a
// control home is configured.
func TestPrepareRunConfig_SandboxHome_Injected(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(taskID string) (string, error) { return sandboxDir, nil },
		ControlHome: "/real/home",
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	base := sharedBuildCacheDir()
	want := []string{
		"SYBRA_HOME=" + sandboxDir,
		"SYBRA_CONTROL_HOME=/real/home",
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build"),
		"GOMODCACHE=" + filepath.Join(base, "go-mod"),
		"npm_config_cache=" + filepath.Join(base, "npm"),
	}
	if len(cfg.ExtraEnv) != len(want) || cfg.ExtraEnv[0] != want[0] || cfg.ExtraEnv[1] != want[1] || cfg.ExtraEnv[2] != want[2] {
		t.Fatalf("ExtraEnv = %v, want %v", cfg.ExtraEnv, want)
	}
}

// TestPrepareRunConfig_SandboxHome_SystemRunSkipsInjection pins that only
// runs with an empty TaskID (system/probe runs) are allowed to skip sandbox
// injection — every task-scoped run must go through it.
func TestPrepareRunConfig_SandboxHome_SystemRunSkipsInjection(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) {
			t.Fatal("resolver must not be called for a system run (empty TaskID)")
			return "", nil
		},
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		Mode: "headless",
		Dir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if len(cfg.ExtraEnv) != 0 {
		t.Fatalf("ExtraEnv = %v, want empty for a system run", cfg.ExtraEnv)
	}
}

// TestPrepareRunConfig_SandboxHome_NilResolverFailsClosed pins that a
// task-scoped run with no resolver configured aborts before spawn rather than
// silently inheriting the ambient/operator SYBRA_HOME.
func TestPrepareRunConfig_SandboxHome_NilResolverFailsClosed(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	m.sandboxHome = nil
	m.mu.Unlock()

	_, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for a task-scoped run with no sandbox resolver")
	}
}

// TestPrepareRunConfig_SandboxHome_ResolverErrorFailsClosed pins that a
// resolver error aborts the run before registration/spawn.
func TestPrepareRunConfig_SandboxHome_ResolverErrorFailsClosed(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return "", errors.New("boom") },
	})

	_, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when the sandbox resolver fails")
	}
}

// TestPrepareRunConfig_SandboxHome_EmptyPathFailsClosed pins that an
// empty-string result from the resolver is rejected rather than silently
// producing SYBRA_HOME= (empty value, which resolves to no override).
func TestPrepareRunConfig_SandboxHome_EmptyPathFailsClosed(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return "  ", nil },
	})

	_, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when the sandbox resolver returns an empty path")
	}
}

// TestPrepareRunConfig_SandboxHome_NonDirectoryFailsClosed pins that a
// resolver result which exists but is not a directory (e.g. a stray file) is
// rejected rather than handed to the subprocess as SYBRA_HOME.
func TestPrepareRunConfig_SandboxHome_NonDirectoryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return file, nil },
	})

	_, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error when the sandbox resolver returns a non-directory path")
	}
}

// TestPrepareRunConfig_SandboxHome_StripsDuplicateCallerEnv pins that a
// caller-supplied SYBRA_HOME/SYBRA_CONTROL_HOME in cfg.ExtraEnv cannot survive
// alongside the trusted values — it must be removed, not merely shadowed, so
// duplicate-key resolution order in the target process can't leak it through.
func TestPrepareRunConfig_SandboxHome_StripsDuplicateCallerEnv(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
		ControlHome: "/real/home",
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
		ExtraEnv: []string{
			"SYBRA_HOME=/attacker/controlled",
			"SYBRA_CONTROL_HOME=/attacker/controlled",
			"OTHER=keep-me",
		},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	base := sharedBuildCacheDir()
	want := []string{
		"OTHER=keep-me",
		"SYBRA_HOME=" + sandboxDir,
		"SYBRA_CONTROL_HOME=/real/home",
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build"),
		"GOMODCACHE=" + filepath.Join(base, "go-mod"),
		"npm_config_cache=" + filepath.Join(base, "npm"),
	}
	if len(cfg.ExtraEnv) != len(want) {
		t.Fatalf("ExtraEnv = %v, want %v", cfg.ExtraEnv, want)
	}
	for i, v := range want {
		if cfg.ExtraEnv[i] != v {
			t.Fatalf("ExtraEnv[%d] = %q, want %q (full: %v)", i, cfg.ExtraEnv[i], v, cfg.ExtraEnv)
		}
	}
}

// TestPrepareRunConfig_SandboxHome_EmptyControlHomeOmitsVar pins that an
// unconfigured ControlHome is simply omitted rather than injected as an empty
// SYBRA_CONTROL_HOME=.
func TestPrepareRunConfig_SandboxHome_EmptyControlHomeOmitsVar(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	base := sharedBuildCacheDir()
	want := []string{
		"SYBRA_HOME=" + sandboxDir,
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build"),
		"GOMODCACHE=" + filepath.Join(base, "go-mod"),
		"npm_config_cache=" + filepath.Join(base, "npm"),
	}
	if len(cfg.ExtraEnv) != len(want) || cfg.ExtraEnv[0] != want[0] || cfg.ExtraEnv[1] != want[1] {
		t.Fatalf("ExtraEnv = %v, want %v", cfg.ExtraEnv, want)
	}
}

func TestPrepareRunConfig_GolangciCache_PerWorktreeAndStripsCaller(t *testing.T) {
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:   "task-1",
		Mode:     "headless",
		Dir:      t.TempDir(),
		ExtraEnv: []string{"GOLANGCI_LINT_CACHE=/attacker/shared-cache"},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	wantCache := filepath.Join(sandboxDir, "golangci-lint-cache")
	var got string
	hits := 0
	for _, kv := range cfg.ExtraEnv {
		if v, ok := strings.CutPrefix(kv, "GOLANGCI_LINT_CACHE="); ok {
			got = v
			hits++
		}
	}
	if hits != 1 {
		t.Fatalf("want exactly one GOLANGCI_LINT_CACHE entry, got %d in %v", hits, cfg.ExtraEnv)
	}
	if got != wantCache {
		t.Fatalf("GOLANGCI_LINT_CACHE = %q, want per-worktree %q", got, wantCache)
	}
	if info, statErr := os.Stat(wantCache); statErr != nil || !info.IsDir() {
		t.Fatalf("cache dir %q not created: %v", wantCache, statErr)
	}
}

func TestPrepareRunConfig_SharedBuildCache_StripsCallerAndShares(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:   "task-1",
		Mode:     "headless",
		Dir:      t.TempDir(),
		ExtraEnv: []string{"GOCACHE=/attacker/cache", "GOMODCACHE=/attacker/mod"},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	base := sharedBuildCacheDir()
	for key, want := range map[string]string{
		"GOCACHE=":          filepath.Join(base, "go-build"),
		"GOMODCACHE=":       filepath.Join(base, "go-mod"),
		"npm_config_cache=": filepath.Join(base, "npm"),
	} {
		var got string
		hits := 0
		for _, kv := range cfg.ExtraEnv {
			if v, ok := strings.CutPrefix(kv, key); ok {
				got = v
				hits++
			}
		}
		if hits != 1 {
			t.Fatalf("want exactly one %s entry, got %d in %v", key, hits, cfg.ExtraEnv)
		}
		if got != want {
			t.Fatalf("%s%q, want shared %q", key, got, want)
		}
		if info, statErr := os.Stat(want); statErr != nil || !info.IsDir() {
			t.Fatalf("shared cache dir %q not created: %v", want, statErr)
		}
	}
}

func TestPrepareRunConfig_GolangciCache_SystemRunSkips(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) {
			t.Fatal("resolver must not be called for a system run")
			return "", nil
		},
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{Mode: "headless", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	for _, kv := range cfg.ExtraEnv {
		if strings.HasPrefix(kv, "GOLANGCI_LINT_CACHE=") {
			t.Fatalf("system run must not inject GOLANGCI_LINT_CACHE, got %v", cfg.ExtraEnv)
		}
	}
}
