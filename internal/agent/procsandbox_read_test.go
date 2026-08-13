package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

func newReadModeManager(readMode string) *Manager {
	return &Manager{
		defaultSandboxMode:     "enforce",
		defaultSandboxReadMode: readMode,
		logger:                 slog.New(slog.DiscardHandler),
	}
}

// specWithWriteRoots builds a minimal enforce spec whose write roots exist on
// disk, so canonicalizeRoot resolves them on both darwin and linux.
func specWithWriteRoots(t *testing.T) sandboxSpec {
	t.Helper()
	return sandboxSpec{
		mode:        "enforce",
		worktree:    t.TempDir(),
		sandboxHome: t.TempDir(),
		tmp:         t.TempDir(),
		sharedCache: t.TempDir(),
		claudeState: t.TempDir(),
	}
}

func TestResolveSandboxReadRoots_GrantsToolchainAndEveryWriteRoot(t *testing.T) {
	m := newReadModeManager("enforce")
	spec := specWithWriteRoots(t)
	cfg := &RunConfig{Role: RoleImplementation, sandbox: spec}

	roots := m.resolveSandboxReadRoots(cfg)

	// Writable roots must also be readable: a run has to read back what it
	// just wrote.
	for _, want := range []string{spec.worktree, spec.sandboxHome, spec.tmp, spec.sharedCache, spec.claudeState} {
		canon, err := canonicalizeRoot(want)
		if err != nil {
			t.Fatalf("canonicalizeRoot(%q): %v", want, err)
		}
		if !slices.Contains(roots, canon) {
			t.Errorf("write root %q missing from read allowlist %v", canon, roots)
		}
	}
	// Spelled out rather than ranged over systemReadRoots, since a test driven
	// by the production list it pins would pass whatever that list became.
	for _, want := range []string{"/usr", "/dev"} {
		if !slices.Contains(roots, want) {
			t.Errorf("%s missing from read allowlist; basic tooling cannot run without it: %v", want, roots)
		}
	}
	if _, err := os.Stat("/nix/store"); err == nil && !slices.Contains(roots, "/nix/store") {
		t.Errorf("/nix/store missing from read allowlist; Nix-backed host tools cannot load their runtime dependencies: %v", roots)
	}
}

func TestAmbientReviewAuthGrantsOnlyGitConfigurationRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	customConfig := filepath.Join(t.TempDir(), "config")
	customGitConfig := filepath.Join(t.TempDir(), "gitconfig")
	for _, path := range []string{
		filepath.Join(customConfig, "github-cli"),
		filepath.Join(customConfig, "git"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(customGitConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	m := &Manager{ghShimDir: shimDir, logger: slog.New(slog.DiscardHandler)}
	cfg := RunConfig{Role: RoleReview, sandbox: specWithWriteRoots(t), ExtraEnv: append(os.Environ(),
		"XDG_CONFIG_HOME="+customConfig,
		"GH_CONFIG_DIR="+filepath.Join(customConfig, "github-cli"),
		"GIT_CONFIG_GLOBAL="+customGitConfig,
	)}
	m.injectAmbientReviewGhShim(&cfg)
	roots := m.resolveSandboxReadRoots(&cfg)
	for _, want := range []string{
		shimDir,
		filepath.Join(customConfig, "github-cli"),
		filepath.Join(customConfig, "git"),
		customGitConfig,
	} {
		if !slices.Contains(roots, want) {
			t.Errorf("ambient review auth root %q missing from read allowlist %v", want, roots)
		}
	}
	if slices.Contains(roots, home) {
		t.Fatalf("ambient review auth granted the whole operator home: %v", roots)
	}
}

func TestAmbientEnvValueEmptyOverrideUsesFallback(t *testing.T) {
	if got := ambientEnvValue([]string{"XDG_CONFIG_HOME="}, "XDG_CONFIG_HOME", "/default/config"); got != "/default/config" {
		t.Fatalf("ambientEnvValue empty override = %q, want fallback", got)
	}
}

func TestAmbientReviewAuthOmitsMissingOptionalConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shimDir := t.TempDir()
	m := &Manager{ghShimDir: shimDir, logger: slog.New(slog.DiscardHandler)}
	cfg := RunConfig{Role: RoleReview, sandbox: specWithWriteRoots(t), ExtraEnv: []string{
		"XDG_CONFIG_HOME=", "GH_CONFIG_DIR=", "GIT_CONFIG_GLOBAL=",
	}}
	m.injectAmbientReviewGhShim(&cfg)

	for _, absent := range []string{
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".config", "git"),
		filepath.Join(home, ".gitconfig"),
	} {
		if slices.Contains(cfg.ReadOnlyPaths, absent) {
			t.Errorf("missing optional config %q was declared as a required read root: %v", absent, cfg.ReadOnlyPaths)
		}
	}
}

func TestResolveSandboxReadRoots_NeverGrantsOpt(t *testing.T) {
	m := newReadModeManager("enforce")
	cfg := &RunConfig{Role: RoleImplementation, sandbox: specWithWriteRoots(t)}

	for _, root := range m.resolveSandboxReadRoots(cfg) {
		// /opt holds the live deploy checkout (/opt/sybra/src) on the server.
		// Granting it re-opens the exact root #2781 exists to close.
		if root == "/opt" || root == "/opt/sybra" || root == "/opt/sybra/src" {
			t.Fatalf("read allowlist grants deploy-checkout root %q", root)
		}
	}
}

func TestResolveSandboxReadRoots_DeterministicCommandOmitsProviderState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper = secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newReadModeManager("enforce")
	cfg := &RunConfig{Role: RoleTestRunner, DisableVerifierControl: true, sandbox: specWithWriteRoots(t)}
	roots := m.resolveSandboxReadRoots(cfg)
	for _, state := range homeStateLinks {
		path := filepath.Join(home, state)
		canon, _ := canonicalizeRoot(path)
		if slices.Contains(roots, path) || (canon != "" && slices.Contains(roots, canon)) {
			t.Fatalf("deterministic command can read provider state %q: %v", state, roots)
		}
	}
	for _, state := range providerHomeRootReadFiles {
		path := filepath.Join(home, state)
		canon, _ := canonicalizeRoot(path)
		if slices.Contains(roots, path) || (canon != "" && slices.Contains(roots, canon)) {
			t.Fatalf("deterministic command can read provider credential %q: %v", state, roots)
		}
	}
	for _, state := range append(append([]string(nil), githubReadSubdirs...), homeRootReadFiles...) {
		path := filepath.Join(home, state)
		canon, _ := canonicalizeRoot(path)
		if slices.Contains(roots, path) || (canon != "" && slices.Contains(roots, canon)) {
			t.Fatalf("deterministic command can read GitHub credential path %q: %v", state, roots)
		}
	}
}

func TestResolveSandboxReadRoots_NativeClaudeInstallIsProviderOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	install := filepath.Join(home, ".local", "share", providerid.Claude)
	if err := os.MkdirAll(install, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newReadModeManager("enforce")
	providerRoots := m.resolveSandboxReadRoots(&RunConfig{Role: RoleReview, sandbox: specWithWriteRoots(t)})
	if !slices.Contains(providerRoots, install) {
		t.Fatalf("native Claude install missing from provider read roots: %v", providerRoots)
	}
	deterministicRoots := m.resolveSandboxReadRoots(&RunConfig{Role: RoleTestRunner, DisableVerifierControl: true, sandbox: specWithWriteRoots(t)})
	if slices.Contains(deterministicRoots, install) {
		t.Fatalf("native Claude install leaked into deterministic read roots: %v", deterministicRoots)
	}
}

func TestResolveSandboxReadRoots_ExternalProviderExecutableIsProviderOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installDir := t.TempDir()
	providerScope := filepath.Join(installDir, "node_modules", "@openai")
	target := filepath.Join(providerScope, providerid.Codex, "bin", providerid.Codex)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(filepath.Dir(target), "codex-code-mode-host")
	if err := os.WriteFile(companion, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	launcher := filepath.Join(binDir, providerid.Codex)
	if err := os.Symlink(target, launcher); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(binDir))
	t.Setenv("PATH", filepath.Base(binDir))
	t.Setenv("GODEBUG", "execerrdot=0")

	m := newReadModeManager("enforce")
	codex := providerByName(providerid.Codex)
	resolvedTarget, err := canonicalizeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	resolvedScope, err := canonicalizeRoot(providerScope)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCompanion, err := canonicalizeRoot(companion)
	if err != nil {
		t.Fatal(err)
	}
	providerRoots := m.resolveSandboxReadRoots(&RunConfig{
		Role: RoleReview, provider: codex, sandbox: specWithWriteRoots(t),
	})
	for _, want := range []string{launcher, resolvedTarget, resolvedScope, resolvedCompanion} {
		if !slices.Contains(providerRoots, want) {
			t.Fatalf("provider executable root %q missing from %v", want, providerRoots)
		}
	}
	deterministicRoots := m.resolveSandboxReadRoots(&RunConfig{
		Role: RoleTestRunner, provider: codex, DisableVerifierControl: true, sandbox: specWithWriteRoots(t),
	})
	for _, denied := range []string{launcher, resolvedTarget, resolvedScope, resolvedCompanion} {
		if slices.Contains(deterministicRoots, denied) {
			t.Fatalf("provider executable root %q leaked into deterministic read roots: %v", denied, deterministicRoots)
		}
	}
}

func TestProviderExecutableReadRoots_RejectsSymlinkedCodexCompanion(t *testing.T) {
	installDir := t.TempDir()
	target := filepath.Join(installDir, providerid.Codex)
	if err := os.WriteFile(target, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-host")
	if err := os.WriteFile(external, []byte("host"), 0o700); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(installDir, "codex-code-mode-host")
	if err := os.Symlink(external, companion); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", installDir)

	roots := providerExecutableReadRoots(providerid.Codex)
	if slices.Contains(roots, companion) || slices.Contains(roots, external) {
		t.Fatalf("symlinked Codex companion leaked into provider roots: %v", roots)
	}
}

func TestClearProviderStateRootsRemovesWritableAndReadableCredentialRoots(t *testing.T) {
	spec := enforceSpec(t.TempDir(), nil, t.TempDir(), t.TempDir(), "", t.TempDir(), "profile", "", gitSandboxRoots{}, gitSandboxOverlay{})
	clearProviderStateRoots(&spec)
	if spec.claudeState != "" || spec.codexState != "" || spec.copilotState != "" || spec.opencodeState != "" || spec.toolCache != "" || spec.appSupport != "" || spec.claudeScratch != "" || len(spec.stateDenied) != 0 {
		t.Fatalf("provider roots survived deterministic clearing: %+v", spec)
	}
	for _, root := range spec.writeRoots() {
		if strings.Contains(root, ".claude") || strings.Contains(root, ".codex") || strings.Contains(root, ".copilot") {
			t.Fatalf("provider credential root survived in writeRoots: %q", root)
		}
	}
}

func TestResolveSandboxReadRoots_BoardOnlyForMonitor(t *testing.T) {
	board := t.TempDir()
	t.Setenv("SYBRA_HOME", board)
	board, err := canonicalizeRoot(board)
	if err != nil {
		t.Fatalf("canonicalizeRoot: %v", err)
	}

	m := newReadModeManager("enforce")
	spec := specWithWriteRoots(t)

	monitor := m.resolveSandboxReadRoots(&RunConfig{Role: RoleMonitor, sandbox: spec})
	if !slices.Contains(monitor, board) {
		t.Errorf("monitor cannot read the board it exists to scan: %v", monitor)
	}

	impl := m.resolveSandboxReadRoots(&RunConfig{Role: RoleImplementation, sandbox: spec})
	if slices.Contains(impl, board) {
		t.Errorf("implementation role granted the whole Sybra board: %v", impl)
	}
}

func TestResolveSandboxReadRoots_GrantsReadOnlyDir(t *testing.T) {
	m := newReadModeManager("enforce")
	spec := specWithWriteRoots(t)
	spec.readOnlyDir = t.TempDir()
	canon, err := canonicalizeRoot(spec.readOnlyDir)
	if err != nil {
		t.Fatalf("canonicalizeRoot: %v", err)
	}

	roots := m.resolveSandboxReadRoots(&RunConfig{Role: RoleHumanReview, sandbox: spec})

	// human-review's deploy-checkout fallback stays readable to be reviewable
	// at all, even though it is never writable.
	if !slices.Contains(roots, canon) {
		t.Errorf("readOnlyDir %q not readable: %v", canon, roots)
	}
}

func TestResolveSandboxReadRoots_GrantsExplicitReadOnlyPaths(t *testing.T) {
	m := newReadModeManager("enforce")
	attempt := t.TempDir()
	cfg := &RunConfig{Role: RoleReview, ReadOnlyPaths: []string{attempt}, sandbox: specWithWriteRoots(t)}
	if !slices.Contains(m.resolveSandboxReadRoots(cfg), attempt) {
		t.Fatalf("explicit attempt root %q was not granted", attempt)
	}
}

func TestApplySandboxReadMode_PostureGatesRestriction(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		wantRoots bool
	}{
		{name: "unset leaves reads unrestricted", mode: "", wantRoots: false},
		{name: "off leaves reads unrestricted", mode: "off", wantRoots: false},
		{name: "report resolves but does not restrict", mode: "report", wantRoots: false},
		{name: "enforce restricts", mode: "enforce", wantRoots: true},
		{name: "invalid degrades to unrestricted", mode: "bogus", wantRoots: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newReadModeManager(tc.mode)
			cfg := &RunConfig{Role: RoleImplementation, sandbox: specWithWriteRoots(t)}

			if err := m.applySandboxReadMode(cfg); err != nil {
				t.Fatalf("applySandboxReadMode: %v", err)
			}
			if got := len(cfg.sandbox.readRoots) > 0; got != tc.wantRoots {
				t.Fatalf("readRoots populated = %v, want %v (roots: %v)", got, tc.wantRoots, cfg.sandbox.readRoots)
			}
		})
	}
}

func TestApplySandboxReadMode_RunConfigOverridesManagerDefault(t *testing.T) {
	m := newReadModeManager("off")
	cfg := &RunConfig{Role: RoleImplementation, SandboxReadMode: "enforce", sandbox: specWithWriteRoots(t)}

	if err := m.applySandboxReadMode(cfg); err != nil {
		t.Fatalf("applySandboxReadMode: %v", err)
	}
	if len(cfg.sandbox.readRoots) == 0 {
		t.Fatal("per-run enforce did not override the manager's off default")
	}
}

func TestSandboxSpecWriteRoots_CoversGitOverlay(t *testing.T) {
	spec := sandboxSpec{
		worktree:            "/wt",
		gitMetadata:         []string{"/clone.git/worktrees/task"},
		gitShared:           []string{"/clone.git/shared"},
		gitReadonly:         []string{"/clone.git/objects"},
		gitOverlayObjectDir: "/home/.overlay/objects",
	}

	got := spec.writeRoots()

	// Overlay dirs missing here stay writable but invisible under read
	// enforcement, failing the run closed on the first git write.
	for _, want := range []string{"/wt", "/clone.git/worktrees/task", "/clone.git/shared", "/clone.git/objects", "/home/.overlay/objects"} {
		if !slices.Contains(got, want) {
			t.Errorf("writeRoots() missing %q: %v", want, got)
		}
	}
}

func TestSandboxSpecWriteRoots_CoversWorkflowSidecarDir(t *testing.T) {
	t.Parallel()
	sidecar := t.TempDir()
	spec := sandboxSpec{sidecarDir: sidecar}
	if !slices.Contains(spec.writeRoots(), sidecar) {
		t.Fatalf("writeRoots() = %v, want workflow sidecar dir %q", spec.writeRoots(), sidecar)
	}
}

func TestResolveSandboxReadRoots_KeepsSymlinkSpellingAndTarget(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	m := newReadModeManager("enforce")
	spec := specWithWriteRoots(t)
	spec.readOnlyDir = link
	roots := m.resolveSandboxReadRoots(&RunConfig{Role: RoleImplementation, sandbox: spec})

	canonLink, err := canonicalizeRoot(link)
	if err != nil {
		t.Fatalf("canonicalizeRoot: %v", err)
	}
	// Granting only the resolved target leaves the link itself out of the mount namespace; on the deploy host /bin is such a link and every "#!/bin/sh" shebang then fails with ENOENT.
	if !slices.Contains(roots, link) {
		t.Errorf("symlink path %q dropped from read allowlist: %v", link, roots)
	}
	if !slices.Contains(roots, canonLink) {
		t.Errorf("symlink target %q dropped from read allowlist: %v", canonLink, roots)
	}
}
