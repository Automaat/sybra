//go:build darwin

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReadProfile_EmptyRootsKeepsBaseProfile(t *testing.T) {
	got, err := buildReadProfile("/tmp/base.sb", nil, t.TempDir())
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}
	// No deployment has read roots today; generating a read-denying profile for them would restrict reads nobody opted into.
	if got != "/tmp/base.sb" {
		t.Fatalf("profile = %q, want the untouched base path", got)
	}
}

func TestBuildReadProfile_DeniesReadsOutsideAllowlist(t *testing.T) {
	path, err := buildReadProfile("/tmp/base.sb", []string{"/usr", "/data/wt"}, t.TempDir())
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	profile := string(raw)

	if !strings.Contains(profile, "(deny file-read*)") {
		t.Errorf("profile does not deny reads by default:\n%s", profile)
	}
	for _, root := range []string{`(subpath "/usr")`, `(subpath "/data/wt")`} {
		if !strings.Contains(profile, root) {
			t.Errorf("profile missing read allow %s:\n%s", root, profile)
		}
	}
	// A plain file such as ~/.claude.json needs literal; subpath alone leaves the provider CLI unauthenticated.
	if !strings.Contains(profile, `(literal "/usr")`) {
		t.Errorf("profile has no literal rules, so file allowlist entries never match:\n%s", profile)
	}
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Errorf("read block clobbered the base write rules:\n%s", profile)
	}
}

func TestSbplQuote_EscapesProfileTerminators(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain path", in: "/data/wt", want: `"/data/wt"`},
		{name: "embedded quote", in: `/data/a"b`, want: `"/data/a\"b"`},
		{name: "embedded backslash", in: `/data/a\b`, want: `"/data/a\\b"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// An unescaped quote closes the literal early, silently widening which paths the profile allows.
			if got := sbplQuote(tc.in); got != tc.want {
				t.Fatalf("sbplQuote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildReadProfile_WritesIntoSandboxHome(t *testing.T) {
	home := t.TempDir()

	first, err := buildReadProfile("/tmp/base.sb", []string{"/usr"}, home)
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}
	second, err := buildReadProfile("/tmp/base.sb", []string{"/usr", "/etc"}, home)
	if err != nil {
		t.Fatalf("buildReadProfile: %v", err)
	}

	// A fresh temp file per spawn would leak one profile per run forever in a long-lived daemon, so repeated builds must reuse one path under the per-task sandbox home.
	if first != second {
		t.Fatalf("profile path changed between runs: %q then %q", first, second)
	}
	if filepath.Dir(first) != home {
		t.Fatalf("profile written to %q, want it inside the sandbox home %q", first, home)
	}
}

func TestBuildReadProfile_RequiresSandboxHome(t *testing.T) {
	// Falling back to the system temp dir is what leaks; an empty home must fail the run closed instead.
	if _, err := buildReadProfile("/tmp/base.sb", []string{"/usr"}, ""); err == nil {
		t.Fatal("buildReadProfile accepted an empty sandbox home")
	}
}
