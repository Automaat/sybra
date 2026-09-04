package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// status_reason records the failing command trimmed to one line and drops the
// diagnostic, so whoever picks the task up next gets a command but not the
// finding. The re-ask path has always built this excerpt; only the escalation
// threw it away.
func TestWriteVerifyDiagnostic(t *testing.T) {
	dir := t.TempDir()
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.setSidecarDirResolverForTest(func(string) (string, error) { return dir, nil })

	const cmd = `GOLANGCI_LINT_CACHE="${TMPDIR:-/tmp}/golangci-lint" mise exec -- golangci-lint run ./internal/...`
	output := strings.Join([]string{
		"some preamble that is not the finding",
		"internal/sybra/app_init.go:1613:15: func (*App).wireWorktreeAccess is unused (unused)",
		"2 issues:",
	}, "\n")

	path := engine.writeVerifyDiagnostic("t1", cmd, output)
	if path == "" {
		t.Fatal("writeVerifyDiagnostic returned no path")
	}
	if got, want := filepath.Dir(path), dir; got != want {
		t.Errorf("wrote to %q, want inside %q", got, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read diagnostic: %v", err)
	}
	if !strings.Contains(string(body), cmd) {
		t.Errorf("diagnostic missing the failing command:\n%s", body)
	}
	// The whole point: the finding itself, not just the command.
	if !strings.Contains(string(body), "wireWorktreeAccess is unused") {
		t.Errorf("diagnostic missing the failure excerpt:\n%s", body)
	}
}

// Without a sidecar dir there is nowhere to write; escalation must still
// proceed rather than being blocked on bookkeeping.
func TestWriteVerifyDiagnostic_NoSidecarDirIsNotFatal(t *testing.T) {
	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	if got := engine.writeVerifyDiagnostic("t1", "cmd", "output"); got != "" {
		t.Errorf("path = %q, want empty when no sidecar dir is resolvable", got)
	}
}

func TestWithDiagnostic(t *testing.T) {
	if got := withDiagnostic("verify failed", ""); got != "verify failed" {
		t.Errorf("with no path = %q, want the reason unchanged", got)
	}
	got := withDiagnostic("verify failed", "/s/.sybra-verify-t1.md")
	if !strings.Contains(got, "/s/.sybra-verify-t1.md") || !strings.HasPrefix(got, "verify failed") {
		t.Errorf("with a path = %q, want the reason plus a pointer", got)
	}
}
