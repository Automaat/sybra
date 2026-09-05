//go:build darwin

package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSandboxRead_RunsHomebrewLinkedBinary pins #3390. Homebrew keeps its
// shared libraries under /opt, which stays out of the read allowlist so the
// deploy checkout below /opt/sybra is unreadable. Granting the binary without
// its libraries grants nothing: the loader aborts before the process runs, and
// every verifier command on such a host failed at git add.
func TestSandboxRead_RunsHomebrewLinkedBinary(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("host sandbox mechanism unavailable; enforce path unexercised on this host")
	}
	var tools []string
	for _, candidate := range []string{"/opt/homebrew/bin/rg", "/opt/homebrew/bin/git", "/opt/homebrew/bin/jq"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			tools = append(tools, candidate)
		}
	}
	if len(tools) == 0 {
		t.Skip("no Homebrew-installed tool on this host")
	}

	worktree := t.TempDir()
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:          "task-homebrew-read",
		Mode:            "headless",
		Dir:             worktree,
		SandboxMode:     "enforce",
		SandboxReadMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}

	// Every Homebrew tool present, not the first one found: git reads its
	// configuration from the prefix and got past the loader only to die on
	// that, so a suite covering one tool proved less than it looked.
	for _, tool := range tools {
		t.Run(filepath.Base(tool), func(t *testing.T) {
			cmd := newProviderCmd(context.Background(), &cfg, false, tool, "--version")
			cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				t.Fatalf("%s failed under the read sandbox: %v: %s", tool, runErr, out)
			}
			if strings.Contains(string(out), "Library not loaded") {
				t.Fatalf("loader could not open a library: %s", out)
			}
		})
	}
}

// TestSandboxRead_KeepsDeployCheckoutUnreadable guards the reason /opt is
// excluded in the first place. Widening to /opt would make the server's live
// source tree readable by every agent.
func TestSandboxRead_KeepsDeployCheckoutUnreadable(t *testing.T) {
	for _, root := range hostRuntimeReadRoots() {
		if !strings.HasPrefix(root, "/opt/homebrew/") {
			t.Errorf("read root %q is outside Homebrew's prefix", root)
		}
	}
	if slices.Contains(hostRuntimeReadRoots(), "/opt") {
		t.Error("/opt granted wholesale; the deploy checkout below /opt/sybra would become readable")
	}
}

// TestSandboxRead_HomebrewRootsReachTheProfile pins that the roots are not
// merely returned but land in the generated profile.
func TestSandboxRead_HomebrewRootsReachTheProfile(t *testing.T) {
	if _, err := os.Stat("/opt/homebrew/lib"); err != nil {
		t.Skip("no Homebrew prefix on this host")
	}
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return t.TempDir(), nil },
	})
	cfg, _, err := m.prepareRunConfig(RunConfig{
		TaskID:          "task-homebrew-roots",
		Mode:            "headless",
		Dir:             t.TempDir(),
		SandboxMode:     "enforce",
		SandboxReadMode: "enforce",
	})
	if err != nil {
		t.Fatalf("prepareRunConfig: %v", err)
	}
	if !slices.Contains(cfg.sandbox.readRoots, "/opt/homebrew/lib") {
		t.Fatalf("Homebrew lib root missing from the read allowlist: %v", cfg.sandbox.readRoots)
	}
	profile, err := os.ReadFile(cfg.sandbox.profilePath)
	if err != nil {
		t.Fatalf("read generated profile: %v", err)
	}
	if !strings.Contains(string(profile), filepath.Clean("/opt/homebrew/lib")) {
		t.Fatal("generated profile does not grant the Homebrew lib root")
	}
}
