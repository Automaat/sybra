package sybra

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

func newEvidenceTestHandler(t *testing.T) (*AgentCompletionHandler, *artifact.Store) {
	t.Helper()
	store := artifact.New(t.TempDir())
	return &AgentCompletionHandler{
		DomainHandler: DomainHandler{logger: discardLogger()},
		artifacts:     store,
	}, store
}

// listEvidenceMetas returns only the imported evidence artifacts (KindGeneric),
// filtering out the progress-log entry that importTestRunnerEvidence appends
// alongside them.
func listEvidenceMetas(t *testing.T, store *artifact.Store, taskID string) []artifact.Meta {
	t.Helper()
	metas, err := store.List(taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []artifact.Meta
	for _, m := range metas {
		if m.Kind == artifact.KindGeneric {
			out = append(out, m)
		}
	}
	return out
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
	stale := filepath.Join(evidenceDir, "stale.png")
	fresh := filepath.Join(evidenceDir, "fresh.png")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(stale, startedAt.Add(-time.Second), startedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, startedAt.Add(time.Second), startedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1", StartedAt: startedAt}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected only fresh evidence imported, got %d: %+v", len(metas), metas)
	}
	if metas[0].SourcePath != fresh {
		t.Fatalf("imported SourcePath = %q, want %q", metas[0].SourcePath, fresh)
	}
}

// TestImportTestRunnerEvidence_ScrubsWorkTypedContent proves the fix for the
// prior adversarial-testing defect: captured evidence for a work-typed task
// must be redacted through scrub.Scrub before it lands in the local artifact
// store, never persisted verbatim. See CLAUDE.md — Work-Data Confidentiality.
func TestImportTestRunnerEvidence_ScrubsWorkTypedContent(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	h.workScrub = func(projectID string) *WorkScrubContext {
		if projectID != "owner/work-repo" {
			return nil
		}
		return &WorkScrubContext{ProjectID: projectID, Blocklist: []string{"konghq/kong-mesh"}}
	}

	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaked := "console error at https://github.com/konghq/kong-mesh/pull/999"
	if err := os.WriteFile(filepath.Join(evidenceDir, "console.log"), []byte(leaked), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "owner/work-repo")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected 1 imported artifact, got %d: %+v", len(metas), metas)
	}
	content, _, err := store.Read(ag.TaskID, metas[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "konghq/kong-mesh") {
		t.Fatalf("work-repo identifier survived unscrubbed in imported evidence: %q", content)
	}
	if !strings.Contains(string(content), "[redacted]") {
		t.Fatalf("expected redaction placeholder in scrubbed content, got %q", content)
	}
}

// TestImportTestRunnerEvidence_NonWorkTypedContentUnscrubbed confirms the
// scrub path is only engaged for work-typed tasks — a nil WorkScrubContext
// (the common case) must leave evidence byte-identical.
func TestImportTestRunnerEvidence_NonWorkTypedContentUnscrubbed(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	h.workScrub = func(string) *WorkScrubContext { return nil }

	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := "console error at https://github.com/example/public-repo/pull/1"
	if err := os.WriteFile(filepath.Join(evidenceDir, "console.log"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	metas := listEvidenceMetas(t, store, ag.TaskID)
	if len(metas) != 1 {
		t.Fatalf("expected 1 imported artifact, got %d: %+v", len(metas), metas)
	}
	content, _, err := store.Read(ag.TaskID, metas[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != original {
		t.Fatalf("expected unscrubbed content %q, got %q", original, content)
	}
}

// TestImportTestRunnerEvidence_SurfacesProgressEntry confirms imported
// evidence is discoverable from the task's progress log, satisfying the
// "surface it on the task" requirement.
func TestImportTestRunnerEvidence_SurfacesProgressEntry(t *testing.T) {
	h, store := newEvidenceTestHandler(t)
	wt := t.TempDir()
	evidenceDir := filepath.Join(wt, worktree.EvidenceDirName)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceDir, "shot.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: "task-1"}
	h.importTestRunnerEvidence(ag, wt, "")

	entries, err := store.ReadProgress(ag.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 progress entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].Kind != artifact.ProgressKindProgress {
		t.Errorf("kind = %q, want %q", entries[0].Kind, artifact.ProgressKindProgress)
	}
	if !strings.Contains(entries[0].Message, "1 test-runner evidence file") {
		t.Errorf("message = %q, want it to mention the imported count", entries[0].Message)
	}
}
