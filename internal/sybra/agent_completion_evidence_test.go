package sybra

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/worktree"
)

func newEvidenceTestHandler(t *testing.T) (*AgentCompletionHandler, *artifact.Store) {
	t.Helper()
	store := artifact.New(t.TempDir())
	return &AgentCompletionHandler{
		DomainHandler: DomainHandler{logger: discardLogger()},
		artifacts:     store,
	}, store
}

func TestImportTestRunnerEvidence_NilArtifactStore(t *testing.T) {
	h := &AgentCompletionHandler{DomainHandler: DomainHandler{logger: discardLogger()}}
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, worktree.EvidenceDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, worktree.EvidenceDirName, "shot.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{ID: "a1", TaskID: "t1"}
	// Must not panic with a nil artifact store.
	h.importTestRunnerEvidence(ag, wt)
}

func TestImportTestRunnerEvidence_MissingDirIsNoop(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	ag := &agent.Agent{ID: "a1", TaskID: "t1"}
	h.importTestRunnerEvidence(ag, t.TempDir())
	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(ag, wt)
	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(ag, wt)

	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(ag, wt)

	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(ag, wt)

	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(ag, wt)

	metas, err := store.List(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
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
	h.importTestRunnerEvidence(first, wt)

	if err := os.WriteFile(filepath.Join(evidenceDir, "shot.png"), []byte("second-run"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := &agent.Agent{ID: "agent-2", TaskID: "task-1"}
	h.importTestRunnerEvidence(second, wt)

	metas, err := store.List("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected both runs' evidence preserved as distinct artifacts, got %d: %+v", len(metas), metas)
	}
}
