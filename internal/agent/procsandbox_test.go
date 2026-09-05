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
	cfg := &RunConfig{sandbox: sandboxSpec{
		mode:        "enforce",
		worktree:    "/private/tmp/wt",
		sandboxHome: "/private/tmp/home",
		tmp:         "/private/tmp",
		tmpAlias:    `^/private/tmp/claude-[^/]+-cwd(/.*)?$`,
	}}
	name, args := wrapInvocation("claude", []string{"-p", "hi"}, cfg)
	if name != sandboxExecPath {
		t.Fatalf("wrapInvocation name = %q, want sandbox-exec path %q", name, sandboxExecPath)
	}
	if len(args) < 2 {
		t.Fatalf("wrapInvocation args = %v, want embedded profile via -p", args)
	}
	if args[0] != "-p" || args[1] != string(agentSandboxProfile) {
		t.Fatalf("wrapInvocation did not pass the embedded profile via -p: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"WORKTREE=/private/tmp/wt",
		"SANDBOX_HOME=/private/tmp/home",
		"TMP=/private/tmp",
		"TMP_ALIAS_PATTERN=^/private/tmp/claude-[^/]+-cwd(/.*)?$",
		"claude -p hi",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args %v missing %q", args, want)
		}
	}
}

func TestSandboxTmpAlias_StableTmpAliasAllowed(t *testing.T) {
	got := sandboxTmpAlias("/private/var/folders/zz/abcd/T")
	if got != `^/private/tmp/claude-[^/]+-cwd(/.*)?$` {
		t.Fatalf("sandboxTmpAlias() = %q, want narrow Claude cwd pattern", got)
	}
}

func TestSandboxTmpAlias_UnrelatedRootDenied(t *testing.T) {
	if got := sandboxTmpAlias("/opt/not-temp"); got != "" {
		t.Fatalf("sandboxTmpAlias() = %q for unrelated root, want empty", got)
	}
}
