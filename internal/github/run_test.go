package github

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRunWithEnv_ExecutesGhAndAppliesEnv exercises the real subprocess path
// (unlike the fakeExecer-swap tests elsewhere in this package): Run/RunWithEnv
// are the sole gated constructor every gh call in the process — including
// cross-package callers like internal/monitor's issue sink and
// review.findMergedPRByBranch — must route through instead of shelling out
// directly, so their traffic is never invisible to the shared rate budget
// (see #2496).
func TestRunWithEnv_ExecutesGhAndAppliesEnv(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	captured := filepath.Join(dir, "captured.txt")
	script := "#!/bin/bash\nprintf '%s|%s' \"$1\" \"$MARKER\" > " + captured + "\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)

	if _, err := RunWithEnv(context.Background(), append(os.Environ(), "MARKER=present"), "issue", "list"); err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured: %v", err)
	}
	if string(got) != "issue|present" {
		t.Fatalf("captured = %q, want %q", got, "issue|present")
	}
}

// TestRunWithEnv_NilEnvInheritsAmbient asserts the nil-env case (no App auth
// configured) leaves the subprocess environment untouched, exactly like
// ghExecer/ghRunCtx's own inline construction did before this was extracted.
func TestRunWithEnv_NilEnvInheritsAmbient(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	captured := filepath.Join(dir, "captured.txt")
	script := "#!/bin/bash\n[[ -n \"$SYBRA_TEST_MARKER\" ]] && printf 'present' > " + captured + "\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SYBRA_TEST_MARKER", "present")

	if _, err := RunWithEnv(context.Background(), nil, "issue", "list"); err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured: %v", err)
	}
	if string(got) != "present" {
		t.Fatalf("ambient env not inherited when env is nil: %q", got)
	}
}

func TestAmbientAuthTokenResolvesHostCredential(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := "#!/bin/bash\n[[ \"$*\" = 'auth token --hostname github.com' ]] || exit 2\nprintf 'ambient-token\\n'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GH_HOST", "enterprise.example")

	token, err := AmbientAuthToken()
	if err != nil {
		t.Fatalf("AmbientAuthToken: %v", err)
	}
	if token != "ambient-token" {
		t.Fatalf("token = %q, want ambient-token", token)
	}
}

func TestAmbientAuthTokenBypassesNetworkAuthCircuit(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	t.Cleanup(DisableAppAuth)
	resetAuthHealthForTest()
	DisableAppAuth()

	// Use an error that the circuit classifies as an ambient credential failure.
	ObserveCallResult(nil, &ambientAuthTestError{})
	before := AuthHealthSnapshot()
	if before.State != AuthUnavailable {
		t.Fatalf("auth state before lookup = %q, want unavailable", before.State)
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := "#!/bin/bash\nprintf 'ambient-token\\n'\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)

	token, err := AmbientAuthToken()
	if err != nil {
		t.Fatalf("AmbientAuthToken: %v", err)
	}
	if token != "ambient-token" {
		t.Fatalf("token = %q, want ambient-token", token)
	}
	after := AuthHealthSnapshot()
	if after.State != AuthUnavailable || after.Transitions != before.Transitions || after.SuppressedCalls != before.SuppressedCalls {
		t.Fatalf("auth health changed during local lookup: before=%+v after=%+v", before, after)
	}
}

type ambientAuthTestError struct{}

func (*ambientAuthTestError) Error() string { return "gh: authentication required, run gh auth login" }

// TestRun_UsesPackageGHEnv asserts Run (unlike RunWithEnv) sources its
// credential env from this package's own GHEnv() rather than requiring the
// caller to supply one — the form findMergedPRByBranch migrated to.
func TestRun_UsesPackageGHEnv(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	t.Cleanup(DisableAppAuth)
	resetAuthHealthForTest()
	DisableAppAuth()

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	captured := filepath.Join(dir, "captured.txt")
	script := "#!/bin/bash\n[[ -n \"$SYBRA_TEST_MARKER\" ]] && printf 'present' > " + captured + "\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SYBRA_TEST_MARKER", "present")

	if _, err := Run(context.Background(), "issue", "list"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured: %v", err)
	}
	if string(got) != "present" {
		t.Fatalf("ambient env not inherited when no App auth is configured: %q", got)
	}
}
