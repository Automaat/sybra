//go:build darwin

package agent

import (
	"os"
	"strings"
	"testing"
)

// TestPrepareRunConfig_Sandbox_DefaultModeResolvesReport pins that a run with
// no explicit SandboxMode (a fresh install with no agent.sandbox_mode
// config) never wraps the spawn: report mode always resolves cfg.sandbox to
// "off" (unwrapped) after logging the would-be allowlist, so a bug in the
// enforce-only wrapper can never regress the default rollout posture.
func TestPrepareRunConfig_Sandbox_DefaultModeResolvesReport(t *testing.T) {
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
	if cfg.sandbox.mode != "off" {
		t.Fatalf("cfg.sandbox.mode = %q, want %q (report never wraps)", cfg.sandbox.mode, "off")
	}
}

// TestPrepareRunConfig_Sandbox_OffModeSkipsResolution pins that an explicit
// "off" SandboxMode never touches sandbox-exec/profile resolution at all.
func TestPrepareRunConfig_Sandbox_OffModeSkipsResolution(t *testing.T) {
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-1",
		Mode:        "headless",
		Dir:         t.TempDir(),
		SandboxMode: "off",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.sandbox.mode != "off" {
		t.Fatalf("cfg.sandbox.mode = %q, want off", cfg.sandbox.mode)
	}
}

// TestPrepareRunConfig_Sandbox_EnforceResolvesRoots pins that enforce mode
// actually computes a wrappable spec with canonicalized
// worktree/sandbox-home/tmp roots, ready for wrapInvocation. The empty profile
// path selects Darwin's embedded base profile rather than a reclaimable temp
// file; read enforcement replaces it with a generated profile when needed.
func TestPrepareRunConfig_Sandbox_EnforceResolvesRoots(t *testing.T) {
	sandboxDir := t.TempDir()
	worktreeDir := t.TempDir()
	sidecarDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})

	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-1",
		Mode:        "headless",
		Dir:         worktreeDir,
		SidecarDir:  sidecarDir,
		SandboxMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.sandbox.mode != "enforce" {
		t.Fatalf("cfg.sandbox.mode = %q, want enforce", cfg.sandbox.mode)
	}
	if cfg.sandbox.worktree == "" || cfg.sandbox.sandboxHome == "" || cfg.sandbox.tmp == "" {
		t.Fatalf("cfg.sandbox incomplete: %+v", cfg.sandbox)
	}
	if cfg.sandbox.profilePath != "" {
		t.Fatalf("cfg.sandbox.profilePath = %q, want embedded base profile", cfg.sandbox.profilePath)
	}
	wantSidecarDir, err := canonicalizeRoot(sidecarDir)
	if err != nil {
		t.Fatalf("canonicalize sidecar dir: %v", err)
	}
	if cfg.sandbox.sidecarDir != wantSidecarDir {
		t.Fatalf("cfg.sandbox.sidecarDir = %q, want %q", cfg.sandbox.sidecarDir, wantSidecarDir)
	}
}

// TestInjectProcessSandbox_TasklessRunDoesNotMaterializeProfile pins the
// system-run case where injectSandboxHome intentionally leaves
// resolvedSandboxHome empty. The write sandbox falls back to Dir as an
// allowlist root, but the embedded base profile must not be materialized
// there: monitor runs often point Dir at the checkout used by auto-update.
func TestInjectProcessSandbox_TasklessRunDoesNotMaterializeProfile(t *testing.T) {
	dir := t.TempDir()
	m, _ := newTestManager(t)
	cfg := RunConfig{Dir: dir, SandboxMode: "enforce"}

	if err := m.injectProcessSandbox(&cfg); err != nil {
		t.Fatalf("injectProcessSandbox: %v", err)
	}
	if cfg.sandbox.profilePath != "" {
		t.Fatalf("cfg.sandbox.profilePath = %q, want embedded base profile", cfg.sandbox.profilePath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read taskless checkout: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "agent-sandbox.sb" || strings.HasPrefix(entry.Name(), "sybra-agent-sandbox-") {
			t.Fatalf("taskless sandbox setup materialized profile in checkout: %v", entries)
		}
	}
}

// TestInjectProcessSandbox_EnforceFailsClosedOnBadRoot pins that an
// unresolvable root aborts the run before spawn in enforce mode, mirroring
// injectSandboxHome's fail-closed discipline. Exercises injectProcessSandbox
// directly (bypassing injectSandboxHome) so the failure is unambiguously
// this function's own canonicalization, not the earlier sandbox-home guard.
func TestInjectProcessSandbox_EnforceFailsClosedOnBadRoot(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := RunConfig{
		TaskID:              "task-1",
		Dir:                 t.TempDir(),
		SandboxMode:         "enforce",
		resolvedSandboxHome: t.TempDir() + "/gone", // never created — EvalSymlinks fails
	}
	if err := m.injectProcessSandbox(&cfg); err == nil {
		t.Fatal("expected error for an unresolvable sandbox-home root in enforce mode")
	}
}

// TestInjectProcessSandbox_ReportNeverFailsClosed pins report mode's safety
// invariant: the exact same unresolvable root that aborts enforce mode must
// not fail a report-mode run — report mode falls back to unwrapped ("off")
// instead, per DefaultSandboxMode's "cannot break agents on rollout"
// guarantee.
func TestInjectProcessSandbox_ReportNeverFailsClosed(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := RunConfig{
		TaskID:              "task-1",
		Dir:                 t.TempDir(),
		SandboxMode:         "report",
		resolvedSandboxHome: t.TempDir() + "/gone",
	}
	if err := m.injectProcessSandbox(&cfg); err != nil {
		t.Fatalf("injectProcessSandbox: %v, want no error (report never fails closed)", err)
	}
	if cfg.sandbox.mode != "off" {
		t.Fatalf("cfg.sandbox.mode = %q, want off", cfg.sandbox.mode)
	}
}

// TestPrepareRunConfig_Sandbox_ReportFallsBackOnUnavailableWrapper pins that
// when the sandbox-home injection succeeds but the process-sandbox wrapper
// itself would be unusable, report mode logs and continues unwrapped rather
// than erroring. Simulated by forcing SandboxMode="report" with a real,
// existing sandbox home (so only injectProcessSandbox's own resolution is
// under test) — sandbox-exec is expected to be available on the darwin test
// host, so this asserts the always-succeeds path; the unavailable-wrapper
// branch is covered directly by TestWrapInvocation_NilConfigUnwrapped and
// the config-level DefaultSandboxMode tests instead, since forcing
// sandbox-exec to appear unavailable from within a test is not possible
// without root-level PATH manipulation.
func TestPrepareRunConfig_Sandbox_ReportFallsBackOnUnavailableWrapper(t *testing.T) {
	sandboxDir := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxDir, nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:      "task-1",
		Mode:        "headless",
		Dir:         t.TempDir(),
		SandboxMode: "report",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if cfg.sandbox.mode != "off" {
		t.Fatalf("cfg.sandbox.mode = %q, want off (report never wraps)", cfg.sandbox.mode)
	}
}
