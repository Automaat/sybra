//go:build darwin

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerificationCommandCanCommit reproduces the verifier failure that
// exposed /dev/null as a required read root: Git opens it read-write while
// launching hooks and rejects an otherwise writable disposable checkout when
// the read profile grants only the literal write rule.
func TestVerificationCommandCanCommit(t *testing.T) {
	if !sandboxExecAvailable() {
		t.Skip("sandbox-exec not installed; enforce path unexercised on this host")
	}
	_, workspace := setupLinkedWorktree(t)
	sandboxHome := filepath.Join(filepath.Dir(workspace), "scratch")
	if err := os.MkdirAll(sandboxHome, 0o700); err != nil {
		t.Fatal(err)
	}
	m, _ := newTestManager(t, ManagerConfig{
		SandboxHome: func(string) (string, error) { return sandboxHome, nil },
	})

	var output bytes.Buffer
	err := m.RunVerificationCommand(t.Context(), RunConfig{
		TaskID:   "task-verification-git",
		Role:     RoleTestRunner,
		Dir:      workspace,
		ExtraEnv: os.Environ(),
	}, "/bin/sh", []string{"-c", "echo verified > verified.txt && git add verified.txt && git commit -q -m verified"}, &output)
	if err != nil {
		t.Fatalf("RunVerificationCommand git commit: %v: %s", err, output.String())
	}
	if strings.Contains(output.String(), "Operation not permitted") {
		t.Fatalf("git commit hit sandbox denial: %s", output.String())
	}
}
