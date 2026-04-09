package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAgentOutputFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentID := "abc12345"

	f, err := NewAgentOutputFile(dir, agentID)
	if err != nil {
		t.Fatalf("NewAgentOutputFile: %v", err)
	}
	defer f.Close()

	if !strings.Contains(f.Name(), agentID) {
		t.Errorf("filename %q does not contain agentID %q", f.Name(), agentID)
	}
	if !strings.HasSuffix(f.Name(), ".ndjson") {
		t.Errorf("filename %q does not end with .ndjson", f.Name())
	}
	// File should be inside the agents/ subdirectory.
	agentsDir := filepath.Join(dir, "agents")
	if !strings.HasPrefix(f.Name(), agentsDir) {
		t.Errorf("file %q not under agents/ dir %q", f.Name(), agentsDir)
	}
}

func TestNewAgentOutputFile_CreatesDir(t *testing.T) {
	t.Parallel()
	// Use a subdirectory that doesn't exist yet.
	dir := filepath.Join(t.TempDir(), "logs", "nested")

	f, err := NewAgentOutputFile(dir, "agt-001")
	if err != nil {
		t.Fatalf("NewAgentOutputFile: %v", err)
	}
	defer f.Close()

	agentsDir := filepath.Join(dir, "agents")
	if _, err := os.Stat(agentsDir); err != nil {
		t.Errorf("agents/ dir not created: %v", err)
	}
}
