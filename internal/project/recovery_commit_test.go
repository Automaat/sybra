package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func isolateGitSigning(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func TestRecoveryCommitArgs(t *testing.T) {
	signedWithIdentity := recoveryCommitArgs("msg", true, true)
	if slices.Contains(signedWithIdentity, "user.email=sybra@localhost") {
		t.Error("should not inject the Sybra identity when the worktree has one")
	}
	if !slices.Contains(signedWithIdentity, "-S") || slices.Contains(signedWithIdentity, "--no-gpg-sign") {
		t.Errorf("want -S and no --no-gpg-sign when signing, got %v", signedWithIdentity)
	}

	keyless := recoveryCommitArgs("msg", true, false)
	if !slices.Contains(keyless, "--no-gpg-sign") || slices.Contains(keyless, "-S") {
		t.Errorf("want --no-gpg-sign and no -S when keyless, got %v", keyless)
	}

	noIdentity := recoveryCommitArgs("msg", false, false)
	if !slices.Contains(noIdentity, "user.name=Sybra") || !slices.Contains(noIdentity, "user.email=sybra@localhost") {
		t.Errorf("want Sybra fallback identity when none configured, got %v", noIdentity)
	}

	for _, args := range [][]string{signedWithIdentity, keyless, noIdentity} {
		if !slices.Contains(args, "--no-verify") || !slices.Contains(args, "--signoff") {
			t.Errorf("recovery commit must always --no-verify --signoff, got %v", args)
		}
	}
}

func TestAutoCommitUncommitted_FallbackIdentityWhenNoneConfigured(t *testing.T) {
	isolateGitSigning(t)

	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("recovered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !AutoCommitUncommitted(context.Background(), dir, "wip: recovered work") {
		t.Fatal("expected AutoCommitUncommitted to commit via the Sybra fallback identity")
	}

	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%an <%ae>%n%(trailers:key=Signed-off-by,valueonly)").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Sybra <sybra@localhost>") {
		t.Errorf("expected Sybra fallback author and signoff, got:\n%s", got)
	}
}
