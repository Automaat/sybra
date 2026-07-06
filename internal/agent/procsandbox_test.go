//go:build darwin

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxExecAvailable(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Fatal("expected sandbox-exec to be available on darwin CI/dev hosts")
	}
}

func TestMaterializeSandboxProfile(t *testing.T) {
	path, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialized profile: %v", err)
	}
	if !strings.Contains(string(data), `(param "WORKTREE")`) {
		t.Fatalf("materialized profile missing WORKTREE param: %s", data)
	}

	// Repeated calls within the same process return the same cached path
	// rather than re-writing a fresh temp file each time.
	path2, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile (2nd): %v", err)
	}
	if path != path2 {
		t.Fatalf("materializeSandboxProfile returned different paths across calls: %q vs %q", path, path2)
	}
}

func TestCanonicalizeRoot_ResolvesTmpSymlink(t *testing.T) {
	// /tmp is a symlink to /private/tmp on darwin — a caller passing the
	// unresolved form must get back the canonical form, or a seatbelt
	// (subpath ...) allow templated from it would silently deny every
	// legitimate write under /tmp in enforce mode (see the spike write-up).
	got, err := canonicalizeRoot("/tmp")
	if err != nil {
		t.Fatalf("canonicalizeRoot(/tmp): %v", err)
	}
	if got != "/private/tmp" {
		t.Fatalf("canonicalizeRoot(/tmp) = %q, want /private/tmp", got)
	}
}

func TestCanonicalizeRoot_ResolvesDotDot(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := canonicalizeRoot(filepath.Join(sub, "..", ".."))
	if err != nil {
		t.Fatalf("canonicalizeRoot: %v", err)
	}
	want, err := canonicalizeRoot(base)
	if err != nil {
		t.Fatalf("canonicalizeRoot(base): %v", err)
	}
	if got != want {
		t.Fatalf("canonicalizeRoot(%q) = %q, want %q", sub+"/../..", got, want)
	}
}

func TestCanonicalizeRoot_EmptyFails(t *testing.T) {
	if _, err := canonicalizeRoot(""); err == nil {
		t.Fatal("expected error for empty root")
	}
	if _, err := canonicalizeRoot("   "); err == nil {
		t.Fatal("expected error for whitespace-only root")
	}
}

func TestCanonicalizeRoot_NonexistentFails(t *testing.T) {
	if _, err := canonicalizeRoot(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error for a nonexistent root (EvalSymlinks cannot resolve it)")
	}
}

func TestWrapInvocation_OffModeUnwrapped(t *testing.T) {
	cfg := &RunConfig{sandbox: sandboxSpec{mode: "off"}}
	name, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	if name != "claude" || len(args) != 2 {
		t.Fatalf("wrapInvocation(off) = (%q, %v), want unwrapped", name, args)
	}
}

func TestWrapInvocation_NilConfigUnwrapped(t *testing.T) {
	name, args := wrapInvocation("claude", []string{"-p", "hi"}, nil)
	if name != "claude" || len(args) != 2 {
		t.Fatalf("wrapInvocation(nil) = (%q, %v), want unwrapped", name, args)
	}
}

func TestWrapInvocation_EnforceModeWraps(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not available")
	}
	profile, err := materializeSandboxProfile()
	if err != nil {
		t.Fatalf("materializeSandboxProfile: %v", err)
	}
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    "/private/tmp/wt",
		sandboxHome: "/private/tmp/home",
		tmp:         "/private/tmp",
		profilePath: profile,
	}}
	name, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	if name != sandboxExecPath {
		t.Fatalf("wrapInvocation name = %q, want sandbox-exec path %q", name, sandboxExecPath)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-f " + profile,
		"WORKTREE=/private/tmp/wt",
		"SANDBOX_HOME=/private/tmp/home",
		"TMP=/private/tmp",
		"claude -p hi",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args %v missing %q", args, want)
		}
	}
}
