package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
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
	scratch := filepath.Join(runDir, "scratch")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
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
		TaskID: "task-local", Role: RoleTestRunner, Dir: workspace, SandboxMode: "off", ExtraEnv: os.Environ(),
	}, "/bin/sh", []string{"-c", `test "$SYBRA_HOME" = "$1" && test "$XDG_CONFIG_HOME" = "$1/.config" && test "$GIT_CONFIG_GLOBAL" = /dev/null && test "$GIT_CONFIG_NOSYSTEM" = 1`, "sh", scratch}, &output)
	if err != nil {
		t.Fatalf("RunVerificationCommand: %v (%s)", err, output.String())
	}
	if !observed.LocalCommand || observed.Provider != "" {
		t.Fatalf("preflight local=%v provider=%q", observed.LocalCommand, observed.Provider)
	}
	if !slices.Contains(observed.ScratchRoots, scratch) {
		t.Fatalf("certified scratch roots %v omit %q", observed.ScratchRoots, scratch)
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
