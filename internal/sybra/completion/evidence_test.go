package completion

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/worktree"
)

func newEvidenceTestHandler(t *testing.T) (*Handler, *artifact.Store) {
	t.Helper()
	store := artifact.New(t.TempDir())
	return &Handler{
		logger:    discardLogger(),
		artifacts: store,
	}, store
}

func listEvidenceMetas(t *testing.T, store *artifact.Store, taskID string) []artifact.Meta {
	t.Helper()
	metas, err := store.List(taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []artifact.Meta
	for i := range metas {
		if metas[i].Kind == artifact.KindGeneric {
			out = append(out, metas[i])
		}
	}
	return out
}

func TestImportTestRunnerEvidence_NilArtifactStore(t *testing.T) {
	h := &Handler{logger: discardLogger()}
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, worktree.EvidenceDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, worktree.EvidenceDirName, "shot.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{ID: "a1", TaskID: "t1"}
	h.importTestRunnerEvidence(ag, wt, "")
}

func TestImportTestRunnerEvidence_MissingDirIsNoop(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	ag := &agent.Agent{ID: "a1", TaskID: "t1"}
	h.importTestRunnerEvidence(ag, t.TempDir(), "")
	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 0 {
		t.Errorf("expected no artifacts imported, got %d", len(metas))
	}
}

func TestImportTestRunnerEvidence_EmptyDirIsNoop(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, worktree.EvidenceDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{ID: "a1", TaskID: "t1"}
	h.importTestRunnerEvidence(ag, wt, "")
	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 0 {
		t.Errorf("expected no artifacts imported, got %d", len(metas))
	}
}

func TestImportTestRunnerEvidence_ImportsRegularFiles(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "screenshot.png"), []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "console.log"), []byte("console output"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 2 {
		t.Fatalf("expected 2 imported artifacts, got %d: %+v", len(metas), metas)
	}
	for _, m := range metas {
		if m.Kind != artifact.KindGeneric {
			t.Errorf("kind = %q, want generic", m.Kind)
		}
		if m.ProducerRole != string(agent.RoleTestRunner) {
			t.Errorf("producer role = %q, want %q", m.ProducerRole, agent.RoleTestRunner)
		}
		if m.SourcePath == "" {
			t.Error("expected non-empty SourcePath")
		}
	}
}

func TestImportTestRunnerEvidence_SkipsDirsAndSymlinks(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(filepath.Join(evidenceDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(evidenceDir, "real.txt"), filepath.Join(evidenceDir, "link.txt")); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected only the regular file to import, got %d: %+v", len(metas), metas)
	}
	if metas[0].SourcePath != filepath.Join(evidenceDir, "real.txt") {
		t.Errorf("unexpected imported file: %+v", metas[0])
	}
}

func TestImportTestRunnerEvidence_SkipsOversizedFiles(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxEvidenceFileSize+1)
	if err := os.WriteFile(filepath.Join(evidenceDir, "huge.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "small.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 || metas[0].Name != sanitizeEvidenceName("agent-1", 0, "small.txt") {
		t.Fatalf("expected only the small file to import, got %+v", metas)
	}
}

func TestImportTestRunnerEvidence_TruncatesAtMaxFiles(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range maxEvidenceFiles + 5 {
		name := filepath.Join(evidenceDir, fmt.Sprintf("file-%03d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != maxEvidenceFiles {
		t.Fatalf("expected exactly %d imported artifacts, got %d", maxEvidenceFiles, len(metas))
	}
}

func TestImportTestRunnerEvidence_RerunDoesNotOverwritePriorImport(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "shot.png"), []byte("first-run"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(first, wt, "")

	if err := os.WriteFile(filepath.Join(evidenceDir, "shot.png"), []byte("second-run"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := &agent.Agent{ID: "agent-2", TaskID: "task-1"}
	h.importTestRunnerEvidence(second, wt, "")

	metas := listEvidenceMetas(t, store, "task-1")
	if len(metas) != 2 {
		t.Fatalf("expected both runs' evidence preserved as distinct artifacts, got %d: %+v", len(metas), metas)
	}
}

func TestImportTestRunnerEvidence_SkipsFilesOlderThanRun(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(evidenceDir, "old.txt")
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	startedAt := time.Now()
	time.Sleep(20 * time.Millisecond)
	newPath := filepath.Join(evidenceDir, "new.txt")
	if err := os.WriteFile(newPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1", StartedAt: startedAt}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 || metas[0].SourcePath != newPath {
		t.Fatalf("expected only new evidence to import, got %+v", metas)
	}
}

func TestImportTestRunnerEvidence_ScrubsKnownTextFilesForWorkTasks(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	h.workScrub = func(projectID string) *WorkScrubContext {
		if projectID != "owner/work-repo" {
			return nil
		}
		return &WorkScrubContext{Blocklist: []string{"konghq/kong-mesh"}}
	}
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "failure in konghq/kong-mesh during test"
	if err := os.WriteFile(filepath.Join(evidenceDir, "console.log"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "owner/work-repo")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected 1 scrubbed artifact, got %+v", metas)
	}
	got, _, err := store.Read(ag.TaskID, metas[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "konghq/kong-mesh") {
		t.Fatalf("scrubbed content still leaks work identifier: %q", string(got))
	}
	if !strings.Contains(string(got), "[redacted]") {
		t.Fatalf("scrubbed content = %q, want redaction marker", string(got))
	}
}

func TestImportTestRunnerEvidence_SkipsBinaryFilesForWorkTasks(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	h.workScrub = func(projectID string) *WorkScrubContext {
		if projectID != "owner/work-repo" {
			return nil
		}
		return &WorkScrubContext{Blocklist: []string{"konghq/kong-mesh"}}
	}
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "screenshot.png"), []byte{0x89, 'P', 'N', 'G', 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "owner/work-repo")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 0 {
		t.Fatalf("expected binary work evidence to be skipped, got %+v", metas)
	}
}

func TestImportTestRunnerEvidence_NilScrubContextImportsAsIs(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	h.workScrub = func(string) *WorkScrubContext { return nil }
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "raw content with konghq/kong-mesh"
	if err := os.WriteFile(filepath.Join(evidenceDir, "console.log"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected raw artifact import, got %+v", metas)
	}
	got, _, err := store.Read(ag.TaskID, metas[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != src {
		t.Fatalf("content = %q, want %q", string(got), src)
	}
}

func TestSurfaceEvidenceImport_AppendsProgressEntry(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}

	h.surfaceEvidenceImport(ag, 3)

	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Kind != artifact.KindProgress {
		t.Fatalf("expected one progress-log artifact, got %+v", metas)
	}
	got, _, err := store.Read(ag.TaskID, metas[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "imported 3 test-runner evidence file(s)") {
		t.Fatalf("progress log = %q, want evidence import message", string(got))
	}
}
