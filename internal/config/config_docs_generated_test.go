package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestConfigDocs_InSyncWithSource is the drift guard for docs/CONFIG.md. It
// runs `go run ./cmd/gen-config-docs` and diffs the regenerated file against
// what is checked in. If a struct tag or doc comment changed in
// internal/config without regenerating, or someone hand-edited the doc,
// this test fails with a "run go generate" message.
func TestConfigDocs_InSyncWithSource(t *testing.T) {
	root := repoRoot(t)
	docPath := filepath.Join(root, "docs", "CONFIG.md")
	prev, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read docs/CONFIG.md: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(docPath, prev, 0o644) })

	cmd := exec.Command("go", "run", "./cmd/gen-config-docs")
	cmd.Dir = root
	// HOME/SYBRA_HOME are baked into path-shaped defaults before the
	// generator normalizes them back to "~/.sybra" — leave the process
	// environment as-is so the normalization path is exercised for real.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./cmd/gen-config-docs: %v\n%s", err, out)
	}

	regen, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read regenerated docs/CONFIG.md: %v", err)
	}
	if !bytes.Equal(regen, prev) {
		t.Errorf("docs/CONFIG.md is stale — run `go generate ./internal/config/...`")
	}
}

// repoRoot walks up from this test file's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", file)
		}
		dir = parent
	}
}
