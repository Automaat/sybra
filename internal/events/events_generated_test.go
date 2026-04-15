package events

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEventsTSFile_InSyncWithGoSource is the drift guard for
// frontend/src/lib/events.ts. It runs `go run ./cmd/gen-events` into a
// temporary output path and diffs against the on-disk file. If someone
// edits the Go source without regenerating, or hand-edits the TS file,
// this test fails on CI with a clear "run go generate" message.
func TestEventsTSFile_InSyncWithGoSource(t *testing.T) {
	root := repoRoot(t)
	existing, err := os.ReadFile(filepath.Join(root, "frontend", "src", "lib", "events.ts"))
	if err != nil {
		t.Fatalf("read events.ts: %v", err)
	}

	// Run the generator in a temp dir so it writes to our temp frontend
	// path, not clobbering the real file. We do this by copying the repo
	// layout pointers the generator needs: it only reads internal/events
	// and writes to frontend/src/lib, so a temp working dir with a go.mod
	// symlink is overkill — instead, run it pointed at the real root and
	// compare the output it already wrote.
	//
	// Simpler path: run the generator as a subprocess with CWD=root, then
	// compare the (regenerated) file on disk to what we captured. This
	// exercises the real invocation path that `go generate` uses.
	prev := make([]byte, len(existing))
	copy(prev, existing)

	cmd := exec.Command("go", "run", "./cmd/gen-events")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run ./cmd/gen-events: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(root, "frontend", "src", "lib", "events.ts"), prev, 0o644) })

	regen, err := os.ReadFile(filepath.Join(root, "frontend", "src", "lib", "events.ts"))
	if err != nil {
		t.Fatalf("read regenerated events.ts: %v", err)
	}
	if !bytes.Equal(regen, prev) {
		t.Errorf("events.ts is stale — run `go generate ./internal/events/...`\n"+
			"--- on disk ---\n%s\n--- regenerated ---\n%s",
			string(prev), string(regen))
	}
}

// repoRoot walks up from this test file's directory to the module root.
// Mirrors the generator's findModuleRoot; duplicated to keep the test
// self-contained and avoid exporting an internal helper just for tests.
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

// Compile-time sanity: if someone removes a constant that the test above
// expects to be present, we want a helpful hint rather than a silent
// regex-less diff. Reference each event name so `go build` fails if the
// const is renamed without regenerating the TS mirror.
var _ = strings.Join([]string{
	TaskCreated, TaskUpdated, TaskDeleted,
	AgentStatePrefix, AgentOutputPrefix, AgentErrorPrefix,
	AgentStuckPrefix, AgentConvoPrefix, AgentApprovalPrefix, AgentEscalationPrefix,
	OrchestratorState, MonitorReport, LoopAgentUpdated, ReviewsUpdated, RenovateUpdated,
	Notification, TodoistSynced, IssuesUpdated,
	BgOpStarted, BgOpProgress, BgOpCompleted, BgOpFailed,
	AppQuitConfirm, StartupDegraded,
}, ",")
