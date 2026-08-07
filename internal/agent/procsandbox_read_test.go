package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
	if !slices.Contains(roots, "/usr") {
		t.Errorf("/usr missing from read allowlist; no provider CLI can exec without it: %v", roots)
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
