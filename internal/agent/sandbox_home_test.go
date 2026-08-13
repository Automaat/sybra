package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/toolledger"
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
		"TMPPREFIX=" + filepath.Join(sandboxDir, "zsh", "zsh"),
		"SYBRA_SCRATCH_HOME=" + filepath.Join(sandboxDir, "scratch-home"),
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build", "task-1"),
		"GOMODCACHE=" + filepath.Join(base, "go-mod"),
		"npm_config_cache=" + filepath.Join(base, "npm"),
	}
	if !slices.Equal(cfg.ExtraEnv, want) {
		t.Fatalf("ExtraEnv = %v, want %v", cfg.ExtraEnv, want)
	}
}

func TestPrepareRunConfig_ScratchEnvironmentStaysOutsideWorktree(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	worktreeDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-scratch",
		Mode:   "headless",
		Dir:    worktreeDir,
		Prompt: "Run the tests.",
		ExtraEnv: []string{
			"SYBRA_SCRATCH_HOME=" + filepath.Join(worktreeDir, "fakehome"),
			"TMPDIR=" + filepath.Join(worktreeDir, "tmp"),
		},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	want := map[string]string{
		"SYBRA_SCRATCH_HOME": filepath.Join(sandboxDir, "scratch-home"),
	}
	for key, wantPath := range want {
		var values []string
		for _, entry := range cfg.ExtraEnv {
			if value, ok := strings.CutPrefix(entry, key+"="); ok {
				values = append(values, value)
			}
		}
		if len(values) != 1 || values[0] != wantPath {
			t.Errorf("%s values = %v, want [%s]", key, values, wantPath)
		}
		if rel, relErr := filepath.Rel(worktreeDir, wantPath); relErr != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s path %q is not outside worktree %q (rel=%q, err=%v)", key, wantPath, worktreeDir, rel, relErr)
		}
		if info, statErr := os.Stat(wantPath); statErr != nil || !info.IsDir() {
			t.Errorf("%s directory %q was not created: %v", key, wantPath, statErr)
		}
	}
	if !slices.Contains(cfg.ExtraEnv, "TMPDIR="+filepath.Join(worktreeDir, "tmp")) {
		t.Fatalf("caller TMPDIR was replaced: %v", cfg.ExtraEnv)
	}
	// zsh appends its own suffix to TMPPREFIX, so the prefix itself is a file
	// stem and only its parent is a directory that has to exist.
	wantPrefix := filepath.Join(sandboxDir, "zsh", "zsh")
	if !slices.Contains(cfg.ExtraEnv, "TMPPREFIX="+wantPrefix) {
		t.Fatalf("TMPPREFIX not pointed at the sandbox home: %v", cfg.ExtraEnv)
	}
	if info, statErr := os.Stat(filepath.Dir(wantPrefix)); statErr != nil || !info.IsDir() {
		t.Fatalf("TMPPREFIX parent %q was not created: %v", filepath.Dir(wantPrefix), statErr)
	}
	if !strings.Contains(cfg.Prompt, "$SYBRA_SCRATCH_HOME") || !strings.Contains(cfg.Prompt, "outside the Git worktree") {
		t.Fatalf("prompt lacks scratch-home guidance: %q", cfg.Prompt)
	}
}

func TestPrepareRunConfig_VerifierUsesAuthenticatedControlChannel(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		Runtime:       ManagerRuntimeConfig{SandboxMode: "report"},
		SandboxHome:   func(string) (string, error) { return sandboxDir, nil },
		ControlHome:   "/real/home",
		ControlTarget: "127.0.0.1:8080",
		ControlToken:  func(taskID, _ string) string { return "secret-for-" + taskID },
	})
	m.SetGHVerifierAppToken(func() string { return "contents-read-pr-write-token" })
	cfg, _, err := m.prepareRunConfig(RunConfig{TaskID: "task-1", Role: RoleReview, Mode: "headless", Dir: t.TempDir(), SandboxMode: "report"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.ExtraEnv, "\n")
	for _, want := range []string{"SYBRA_CONTROL_API_ONLY=1", "SYBRA_SERVER_TARGET=127.0.0.1:8080", "SYBRA_AUTH_TOKEN_FILE=" + VerifierControlTokenPath(sandboxDir)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("verifier environment lacks %q: %v", want, cfg.ExtraEnv)
		}
	}
	if strings.Contains(joined, "SYBRA_AUTH_TOKEN=") {
		t.Fatalf("verifier bearer leaked into process environment: %v", cfg.ExtraEnv)
	}
	token, err := os.ReadFile(VerifierControlTokenPath(sandboxDir))
	if err != nil || strings.TrimSpace(string(token)) != "secret-for-task-1" {
		t.Fatalf("lease-private verifier credential = %q, err=%v", token, err)
	}
	if strings.Contains(joined, "SYBRA_CONTROL_HOME=") {
		t.Fatalf("verifier received direct operator-store access: %v", cfg.ExtraEnv)
	}
}

func TestInjectSandboxHome_DeterministicVerifierGetsNoControlCapability(t *testing.T) {
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome:   func(string) (string, error) { return sandboxDir, nil },
		ControlHome:   "/real/home",
		ControlTarget: "127.0.0.1:8080",
		ControlToken:  func(string, string) string { return "must-not-be-issued" },
	})
	cfg := RunConfig{TaskID: "task-local", Role: RoleTestRunner, DisableVerifierControl: true}
	if err := m.injectSandboxHome(&cfg); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.ExtraEnv, "\n")
	for _, forbidden := range []string{"SYBRA_CONTROL_HOME=", "SYBRA_CONTROL_API_ONLY=", "SYBRA_SERVER_TARGET=", "SYBRA_AUTH_TOKEN=", "SYBRA_AUTH_TOKEN_FILE="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("deterministic verifier received %q: %v", forbidden, cfg.ExtraEnv)
		}
	}
}

func TestPreparedScratchRootsIncludeConcreteCacheDirectories(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	cfg := RunConfig{TaskID: "task-cache", resolvedSandboxHome: sandboxDir}

	got := preparedScratchRoots(cfg)
	base := sharedBuildCacheDir()
	for _, want := range []string{
		sandboxDir,
		filepath.Join(sandboxDir, "golangci-lint-cache"),
		filepath.Join(base, "go-build", "task-cache"),
		filepath.Join(base, "go-mod"),
		filepath.Join(base, "npm"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("scratch roots %v do not contain %q", got, want)
		}
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

// TestPrepareRunConfig_SandboxHome_IsolatedSystemRun pins the orchestrator
// hardening: selected taskless system agents still get a sandbox SYBRA_HOME, so
// stale sybra-cli/source invocations cannot rewrite the operator's real home.
func TestPrepareRunConfig_SandboxHome_IsolatedSystemRun(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	var gotKey string
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(key string) (string, error) {
			gotKey = key
			return sandboxDir, nil
		},
		ControlHome: "/real/home",
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		Name:        "Sybra Orchestrator",
		Mode:        "headless",
		Dir:         t.TempDir(),
		IsolateHome: true,
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if gotKey != "system-sybra-orchestrator" {
		t.Fatalf("sandbox key = %q, want system-sybra-orchestrator", gotKey)
	}
	base := sharedBuildCacheDir()
	want := []string{
		"SYBRA_HOME=" + sandboxDir,
		"SYBRA_CONTROL_HOME=/real/home",
		"TMPPREFIX=" + filepath.Join(sandboxDir, "zsh", "zsh"),
		"SYBRA_SCRATCH_HOME=" + filepath.Join(sandboxDir, "scratch-home"),
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build", "system-sybra-orchestrator"),
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
	if cfg.resolvedSandboxHome != sandboxDir {
		t.Fatalf("resolvedSandboxHome = %q, want %q", cfg.resolvedSandboxHome, sandboxDir)
	}
	if cfg.sandboxKey != "system-sybra-orchestrator" {
		t.Fatalf("sandboxKey = %q, want system-sybra-orchestrator", cfg.sandboxKey)
	}
}

func TestSystemSandboxKey_EmptyIdentity(t *testing.T) {
	if got := systemSandboxKey(&RunConfig{}); got != "system-run" {
		t.Fatalf("systemSandboxKey(empty) = %q, want system-run", got)
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
			"SYBRA_SCRATCH_HOME=/attacker/controlled",
			"TMPDIR=/attacker/controlled",
			"TMP=/attacker/controlled",
			"TEMP=/attacker/controlled",
			"OTHER=keep-me",
		},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	base := sharedBuildCacheDir()
	want := []string{
		"TMPDIR=/attacker/controlled",
		"TMP=/attacker/controlled",
		"TEMP=/attacker/controlled",
		"OTHER=keep-me",
		"SYBRA_HOME=" + sandboxDir,
		"SYBRA_CONTROL_HOME=/real/home",
		"TMPPREFIX=" + filepath.Join(sandboxDir, "zsh", "zsh"),
		"SYBRA_SCRATCH_HOME=" + filepath.Join(sandboxDir, "scratch-home"),
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build", "task-1"),
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
		"TMPPREFIX=" + filepath.Join(sandboxDir, "zsh", "zsh"),
		"SYBRA_SCRATCH_HOME=" + filepath.Join(sandboxDir, "scratch-home"),
		"GOLANGCI_LINT_CACHE=" + filepath.Join(sandboxDir, "golangci-lint-cache"),
		"GOCACHE=" + filepath.Join(base, "go-build", "task-1"),
		"GOMODCACHE=" + filepath.Join(base, "go-mod"),
		"npm_config_cache=" + filepath.Join(base, "npm"),
	}
	if len(cfg.ExtraEnv) != len(want) || cfg.ExtraEnv[0] != want[0] || cfg.ExtraEnv[1] != want[1] {
		t.Fatalf("ExtraEnv = %v, want %v", cfg.ExtraEnv, want)
	}
}

func TestPrepareRunConfig_GitHubAppTokenNotSnapshottedIntoAgentEnv(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	m.SetGHAppToken(func() string { return "fresh-but-short-lived-token" })

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:   "task-1",
		Mode:     "headless",
		Dir:      t.TempDir(),
		ExtraEnv: []string{"GH_TOKEN=stale-caller-token", "GITHUB_TOKEN=stale-caller-token"},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	var ghToken, githubToken string
	var ghTokenHits, githubTokenHits int
	for _, kv := range cfg.ExtraEnv {
		switch {
		case strings.HasPrefix(kv, "GH_TOKEN="):
			ghTokenHits++
			ghToken = strings.TrimPrefix(kv, "GH_TOKEN=")
		case strings.HasPrefix(kv, "GITHUB_TOKEN="):
			githubTokenHits++
			githubToken = strings.TrimPrefix(kv, "GITHUB_TOKEN=")
		}
		if strings.Contains(kv, "fresh-but-short-lived-token") || strings.Contains(kv, "stale-caller-token") {
			t.Fatalf("agent env leaked a raw GitHub token in %q (full env: %v)", kv, cfg.ExtraEnv)
		}
	}
	if ghTokenHits != 1 || githubTokenHits != 1 {
		t.Fatalf("GH_TOKEN/GITHUB_TOKEN overrides count = %d/%d, want exactly one each (env=%v)", ghTokenHits, githubTokenHits, cfg.ExtraEnv)
	}
	if ghToken != "" || githubToken != "" {
		t.Fatalf("GH_TOKEN/GITHUB_TOKEN = %q/%q, want empty overrides (env=%v)", ghToken, githubToken, cfg.ExtraEnv)
	}
}

func TestPrepareRunConfig_GitHubAppTokenForcesGitCredentialHelper(t *testing.T) {
	fakeGhOnPath(t)
	shimDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
		GhShimDir:   shimDir,
	})
	m.SetGHAppToken(func() string { return "fresh-but-short-lived-token" })

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID: "task-1",
		Mode:   "headless",
		Dir:    t.TempDir(),
		ExtraEnv: []string{
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
			"GIT_CONFIG_VALUE_0=!gh auth git-credential",
			"GIT_CONFIG_PARAMETERS=credential.helper=store",
		},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	want := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=sybra",
		"GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath",
		"GIT_CONFIG_VALUE_1=false",
	}
	for _, kv := range want {
		if !slices.Contains(cfg.ExtraEnv, kv) {
			t.Fatalf("ExtraEnv missing %q: %v", kv, cfg.ExtraEnv)
		}
	}
	for _, kv := range cfg.ExtraEnv {
		if strings.Contains(kv, "!gh auth git-credential") ||
			strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			t.Fatalf("stale git credential config survived in %q (env=%v)", kv, cfg.ExtraEnv)
		}
		if strings.Contains(kv, "fresh-but-short-lived-token") {
			t.Fatalf("agent env leaked raw token in %q (env=%v)", kv, cfg.ExtraEnv)
		}
	}
	path := sandboxTestEnvValue(cfg.ExtraEnv, "PATH")
	if !strings.HasPrefix(path, shimDir+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want shim dir first", path)
	}
}

func sandboxTestEnvValue(env []string, key string) string {
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, key+"="); ok {
			return value
		}
	}
	return ""
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
		"GOCACHE=":          filepath.Join(base, "go-build", "task-1"),
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
			t.Fatalf("%s%q, want %q", key, got, want)
		}
		if info, statErr := os.Stat(want); statErr != nil || !info.IsDir() {
			t.Fatalf("shared cache dir %q not created: %v", want, statErr)
		}
	}
}

func TestPrepareRunConfig_SharedBuildCache_GOCACHEIsPerTask(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	task1, _, err := m.prepareRunConfig(RunConfig{TaskID: "task-1", Mode: "headless", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareRunConfig(task-1): %v", err)
	}
	task2, _, err := m.prepareRunConfig(RunConfig{TaskID: "task-2", Mode: "headless", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareRunConfig(task-2): %v", err)
	}

	var gocache1, gocache2, gomod1, gomod2 string
	for _, kv := range task1.ExtraEnv {
		if v, ok := strings.CutPrefix(kv, "GOCACHE="); ok {
			gocache1 = v
		}
		if v, ok := strings.CutPrefix(kv, "GOMODCACHE="); ok {
			gomod1 = v
		}
	}
	for _, kv := range task2.ExtraEnv {
		if v, ok := strings.CutPrefix(kv, "GOCACHE="); ok {
			gocache2 = v
		}
		if v, ok := strings.CutPrefix(kv, "GOMODCACHE="); ok {
			gomod2 = v
		}
	}
	if gocache1 == "" || gocache2 == "" {
		t.Fatalf("missing GOCACHE values: %q %q", gocache1, gocache2)
	}
	if gocache1 == gocache2 {
		t.Fatalf("GOCACHE must be task-scoped, got shared %q", gocache1)
	}
	if gomod1 == "" || gomod2 == "" {
		t.Fatalf("missing GOMODCACHE values: %q %q", gomod1, gomod2)
	}
	if gomod1 != gomod2 {
		t.Fatalf("GOMODCACHE must stay shared, got %q vs %q", gomod1, gomod2)
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

// TestPrepareRunConfig_NamedBoardReplacesTheControlHome pins the shape a
// task-scoped agent's CLI depends on.
//
// SYBRA_CONTROL_HOME must be absent once a board is named. It points the CLI at
// the operator home, whose config the CLI loads before it ever looks at a
// target — and that home is off the sandbox read allowlist for every role but
// monitor, so under an enforcing read sandbox the CLI dies on the config file
// and never reaches the board it was handed.
func TestPrepareRunConfig_NamedBoardReplacesTheControlHome(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
		ControlHome: "/real/home",
	})
	m.SetBoard("127.0.0.1:9931", "board-secret", "")

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:   "task-1",
		Mode:     "headless",
		Dir:      t.TempDir(),
		ExtraEnv: []string{"SYBRA_SERVER_TARGET=192.0.2.9:1", "SYBRA_CONTROL_HOME=/attacker"},
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	joined := strings.Join(cfg.ExtraEnv, "\n")

	tokenPath := filepath.Join(sandboxDir, boardTokenFile)
	for _, want := range []string{
		"SYBRA_SERVER_TARGET=127.0.0.1:9931",
		"SYBRA_AUTH_TOKEN_FILE=" + tokenPath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment lacks %q: %v", want, cfg.ExtraEnv)
		}
	}
	if strings.Contains(joined, "SYBRA_CONTROL_HOME") {
		t.Errorf("named board still exported a control home; the CLI loads that home's config first: %v", cfg.ExtraEnv)
	}
	if strings.Contains(joined, "192.0.2.9") || strings.Contains(joined, "/attacker") {
		t.Errorf("caller-supplied target survived: %v", cfg.ExtraEnv)
	}
	if strings.Contains(joined, "board-secret") {
		t.Errorf("board token leaked into the process environment, which is world-readable: %v", cfg.ExtraEnv)
	}

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read board token: %v", err)
	}
	if string(data) != "board-secret" {
		t.Errorf("board token = %q, want %q", data, "board-secret")
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat board token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("board token mode = %04o, want 0600", perm)
	}
}

// TestPrepareRunConfig_TLSBoardShipsTheCertificateIntoTheSandbox pins the other
// half: a board serving TLS signs its own certificate, and an agent cannot read
// the operator's copy, so it is copied in beside the token.
func TestPrepareRunConfig_TLSBoardShipsTheCertificateIntoTheSandbox(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	certSrc := filepath.Join(t.TempDir(), "board.pem")
	if err := os.WriteFile(certSrc, []byte("-----BEGIN CERTIFICATE-----\nnot-a-real-cert\n"), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
		ControlHome: "/real/home",
	})
	m.SetBoard("https://127.0.0.1:8443", "board-secret", certSrc)

	cfg, _, err := m.prepareRunConfig(RunConfig{TaskID: "task-1", Mode: "headless", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	caPath := filepath.Join(sandboxDir, boardCAFile)
	if !slices.Contains(cfg.ExtraEnv, "SYBRA_SERVER_CA="+caPath) {
		t.Fatalf("environment lacks the certificate the agent must pin: %v", cfg.ExtraEnv)
	}
	got, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read copied certificate: %v", err)
	}
	if !strings.Contains(string(got), "not-a-real-cert") {
		t.Errorf("copied certificate = %q, want the board's", got)
	}
}

// TestPrepareRunConfig_UnnamedBoardKeepsTheControlHome pins the fallback: with
// no board named there is nothing to point the CLI at, so the control home is
// still the only thing that resolves one.
func TestPrepareRunConfig_UnnamedBoardKeepsTheControlHome(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
		ControlHome: "/real/home",
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{TaskID: "task-1", Mode: "headless", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	joined := strings.Join(cfg.ExtraEnv, "\n")
	if !strings.Contains(joined, "SYBRA_CONTROL_HOME=/real/home") {
		t.Errorf("environment lacks the control home: %v", cfg.ExtraEnv)
	}
	if strings.Contains(joined, "SYBRA_SERVER_TARGET") {
		t.Errorf("named a target with no board set: %v", cfg.ExtraEnv)
	}
}

// TestRecordToolCall_UnsetLedgerDoesNotPanic pins a hazard the interface
// introduced.
//
// The field used to be a *toolledger.Logger whose own methods tolerated a nil
// receiver. As an interface an unset ledger is a nil interface, and calling
// through it panics — on the provider stream path, which takes the whole run
// with it. Every manager built without a ledger is in this state.
func TestRecordToolCall_UnsetLedgerDoesNotPanic(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{})
	m.recordToolCall(toolledger.Record{AgentID: "ag1", Tool: "Bash"})
}
