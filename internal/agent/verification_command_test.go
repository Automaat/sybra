package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationCommandUsesLeaseScratchAndNoProviderCapacity(t *testing.T) {
	t.Setenv("SYBRA_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/operator/secret/gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("XDG_CONFIG_HOME", "/operator/secret/config")
	runDir := t.TempDir()
	workspace := filepath.Join(runDir, "source")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := newTestManager(t)
	var observed RunEnvironment
	m.SetRunEnvironmentPreflight(func(_ context.Context, environment RunEnvironment) error {
		observed = environment
		return nil
	})
	var output bytes.Buffer
	err := m.RunVerificationCommand(t.Context(), RunConfig{
		TaskID: "task-local", Role: RoleTestRunner, Dir: workspace, GitRoots: []string{workspace}, SandboxMode: "off", ExtraEnv: os.Environ(),
	}, "/bin/sh", []string{"-c", `test -d "$SYBRA_HOME" && test "$XDG_CONFIG_HOME" = "$SYBRA_HOME/.config" && test "$GIT_CONFIG_GLOBAL" = /dev/null && test "$GIT_CONFIG_NOSYSTEM" = 1`}, &output)
	if err != nil {
		t.Fatalf("RunVerificationCommand: %v (%s)", err, output.String())
	}
	if !observed.LocalCommand || observed.Provider != "" {
		t.Fatalf("preflight local=%v provider=%q", observed.LocalCommand, observed.Provider)
	}
	if len(observed.GitRoots) != 1 || observed.GitRoots[0] != workspace {
		t.Fatalf("preflight GitRoots = %v, want [%q]", observed.GitRoots, workspace)
	}
	var scratch string
	for _, root := range observed.ScratchRoots {
		if strings.HasPrefix(filepath.Base(root), "sybra-verify-scratch-") {
			scratch = root
			break
		}
	}
	if scratch == "" {
		t.Fatalf("certified scratch roots %v omit the ephemeral verification home", observed.ScratchRoots)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("ephemeral verification home was not removed: stat error = %v", err)
	}
}

func TestVerificationCommandCreatesScratchWhenSiblingIsAbsent(t *testing.T) {
	runDir := t.TempDir()
	workspace := filepath.Join(runDir, "source")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := newTestManager(t)
	var output bytes.Buffer
	if err := m.RunVerificationCommand(t.Context(), RunConfig{
		TaskID: "task-no-scratch", Role: RoleTestRunner, Dir: workspace, ExtraEnv: os.Environ(),
	}, "/bin/sh", []string{"-c", `test -d "$SYBRA_HOME"`}, &output); err != nil {
		t.Fatalf("RunVerificationCommand without sibling scratch: %v (%s)", err, output.String())
	}
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "sybra-verify-scratch-") {
			t.Fatalf("verification scratch leaked after command: %s", entry.Name())
		}
	}
}

func TestVerificationCommandRemovesScratchAfterPreparationFailure(t *testing.T) {
	workspace := t.TempDir()
	m, _ := newTestManager(t)
	var scratch string
	m.SetRunEnvironmentPreflight(func(_ context.Context, environment RunEnvironment) error {
		for _, root := range environment.ScratchRoots {
			if strings.HasPrefix(filepath.Base(root), "sybra-verify-scratch-") {
				scratch = root
			}
		}
		return errors.New("preflight rejected")
	})
	err := m.RunVerificationCommand(t.Context(), RunConfig{
		TaskID: "task-preflight-failure", Role: RoleTestRunner, Dir: workspace, ExtraEnv: os.Environ(),
	}, "/bin/sh", []string{"-c", "exit 0"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "preflight rejected") {
		t.Fatalf("RunVerificationCommand error = %v, want preflight failure", err)
	}
	if scratch == "" {
		t.Fatal("preflight did not observe ephemeral verification home")
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("ephemeral verification home survived failure: stat error = %v", err)
	}
}

func TestRemoveVerificationHomeNeverSilentlyLeavesLockedState(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "state"), []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if err := removeVerificationHome(root); err != nil {
		// Linux denies traversal of the deliberately locked directory. The
		// production caller joins this error into the command result. Restore
		// permissions only so testing.TempDir can clean its fixture.
		if chmodErr := os.Chmod(locked, 0o700); chmodErr != nil {
			t.Fatalf("restore test directory after reported cleanup failure: %v (cleanup error: %v)", chmodErr, err)
		}
		return
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("verification home survived cleanup: stat error = %v", err)
	}
}

func TestIsolateVerifierGitCredentialsDropsAmbientPublishPaths(t *testing.T) {
	scratch := t.TempDir()
	cfg := RunConfig{resolvedSandboxHome: scratch, ExtraEnv: []string{
		"GH_TOKEN=secret", "GITHUB_TOKEN=secret", "SSH_AUTH_SOCK=/agent.sock",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=sybra",
		"GIT_CONFIG_GLOBAL=/operator/.gitconfig", "XDG_CONFIG_HOME=/operator/.config",
	}}
	if err := isolateVerifierGitCredentials(&cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GH_TOKEN": "", "GITHUB_TOKEN": "", "SSH_AUTH_SOCK": "",
		"GIT_CONFIG_COUNT": "0", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"XDG_CONFIG_HOME": filepath.Join(scratch, ".config"),
	}
	for key, value := range want {
		if got := verificationEnvValue(cfg.ExtraEnv, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	for _, assignment := range cfg.ExtraEnv {
		if strings.HasPrefix(assignment, "GIT_CONFIG_KEY_") || strings.HasPrefix(assignment, "GIT_CONFIG_VALUE_") {
			t.Fatalf("ambient Git credential helper survived: %q", assignment)
		}
	}
}

func TestReviewRoleReceivesOnlyVerifierGitHubToken(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetGHAppToken(func() string { return "full-token" })
	m.SetGHVerifierAppToken(func() string { return "contents-read-pr-write-token" })
	cfg := RunConfig{Role: RoleReview, Dir: t.TempDir(), resolvedSandboxHome: t.TempDir(), ExtraEnv: os.Environ()}
	if err := m.injectGitAccess(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := verificationEnvValue(cfg.ExtraEnv, "GH_TOKEN"); got != "contents-read-pr-write-token" {
		t.Fatalf("review GH_TOKEN = %q", got)
	}
	for _, assignment := range cfg.ExtraEnv {
		if strings.Contains(assignment, "full-token") {
			t.Fatalf("full-capability token leaked to review role: %q", assignment)
		}
	}

	testCfg := RunConfig{Role: RoleTestRunner, Dir: t.TempDir(), resolvedSandboxHome: t.TempDir(), ExtraEnv: os.Environ()}
	if err := m.injectGitAccess(&testCfg); err != nil {
		t.Fatal(err)
	}
	if got := verificationEnvValue(testCfg.ExtraEnv, "GH_TOKEN"); got != "" {
		t.Fatalf("test-runner GH_TOKEN = %q, want no GitHub mutation capability", got)
	}
}

func TestTaskScopedReviewFailsWithoutRestrictedGitHubToken(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := RunConfig{TaskID: "task-review", Role: RoleReview, Dir: t.TempDir(), resolvedSandboxHome: t.TempDir(), ExtraEnv: os.Environ()}
	err := m.injectGitAccess(&cfg)
	if err == nil || !strings.Contains(err.Error(), "restricted GitHub App verifier token") {
		t.Fatalf("injectGitAccess error = %v, want clear restricted-token failure", err)
	}
}

func TestTaskScopedReviewAllowsAmbientGitHubAuthWhenConfigured(t *testing.T) {
	shimDir := t.TempDir()
	m := &Manager{allowAmbientReviewAuth: true, ghShimDir: shimDir, logger: discardLogger()}
	cfg := RunConfig{
		TaskID: "task-review", Role: RoleReview, Dir: t.TempDir(), resolvedSandboxHome: t.TempDir(),
		ExtraEnv: []string{"GH_TOKEN=operator-token", "GITHUB_TOKEN=operator-token"},
	}
	if err := m.injectGitAccess(&cfg); err != nil {
		t.Fatalf("injectGitAccess() error = %v", err)
	}
	if got := verificationEnvValue(cfg.ExtraEnv, "GH_TOKEN"); got != "operator-token" {
		t.Fatalf("GH_TOKEN = %q, want ambient operator credential", got)
	}
	for _, key := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"} {
		if got := verificationEnvValue(cfg.ExtraEnv, key); got != "" {
			t.Fatalf("%s = %q, want no Sybra credential-helper override", key, got)
		}
	}
}

func TestTaskScopedReviewPrefersRestrictedGitHubTokenOverAmbientAuth(t *testing.T) {
	m := &Manager{
		allowAmbientReviewAuth: true,
		ghVerifierAppToken:     func() string { return "restricted-token" },
		logger:                 discardLogger(),
	}
	cfg := RunConfig{
		TaskID: "task-review", Role: RoleReview, Dir: t.TempDir(), resolvedSandboxHome: t.TempDir(),
		ExtraEnv: []string{"GH_TOKEN=operator-token", "GITHUB_TOKEN=operator-token"},
	}
	if err := m.injectGitAccess(&cfg); err != nil {
		t.Fatalf("injectGitAccess() error = %v", err)
	}
	if got := verificationEnvValue(cfg.ExtraEnv, "GH_TOKEN"); got != "restricted-token" {
		t.Fatalf("GH_TOKEN = %q, want restricted verifier credential", got)
	}
	if got := verificationEnvValue(cfg.ExtraEnv, "GIT_CONFIG_GLOBAL"); got != "/dev/null" {
		t.Fatalf("GIT_CONFIG_GLOBAL = %q, want verifier credential isolation", got)
	}
}

func verificationEnvValue(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, assignment := range env {
		if found, ok := strings.CutPrefix(assignment, prefix); ok {
			value = found
		}
	}
	return value
}
