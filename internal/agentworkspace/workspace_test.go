package agentworkspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/notes"
)

func TestPrepareUsesImmutableBaseAndCollectsDeterministicHandback(t *testing.T) {
	source, base := repository(t)
	advanceFile := filepath.Join(source, "later.txt")
	if err := os.WriteFile(advanceFile, []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, source, "add", "later.txt")
	git(t, source, "commit", "-m", "advance ref")

	spec := runSpec(base, true)
	layout, err := Prepare(t.Context(), filepath.Join(t.TempDir(), "runs"), source, spec)
	if err != nil {
		t.Fatal(err)
	}
	head, err := gitexec.Output(t.Context(), gitexec.Options{Dir: layout.Worktree}, "rev-parse", "HEAD")
	if err != nil || head != base {
		t.Fatalf("HEAD = %q, %v; want immutable %s", head, err, base)
	}
	if _, err := os.Stat(filepath.Join(layout.Worktree, "later.txt")); !os.IsNotExist(err) {
		t.Fatalf("mutable ref content present: %v", err)
	}
	if info, err := os.Stat(filepath.Join(layout.Worktree, notes.FileName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private notes = %v, %v", info, err)
	}

	if err := os.WriteFile(filepath.Join(layout.Worktree, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Worktree, "z.txt"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Worktree, "a.txt"), []byte("a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Worktree, notes.FileName), []byte("PRIVATE CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest1, content1, err := Collect(t.Context(), layout, spec, "test")
	if err != nil {
		t.Fatal(err)
	}
	manifest2, content2, err := Collect(t.Context(), layout, spec, "test")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := executioncontract.ValidateArtifactPackage(manifest1, content1)
	if err != nil {
		t.Fatal(err)
	}
	manifest1.GeneratedAt, manifest2.GeneratedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(manifest1, manifest2) || !reflect.DeepEqual(content1, content2) {
		t.Fatal("repeated collection was not deterministic")
	}
	if strings.Contains(string(content1), "PRIVATE CANARY") {
		t.Fatal("private working memory leaked into handback")
	}
	wantPaths := []string{"git/dirty.patch", "a.txt", "z.txt"}
	gotPaths := make([]string, len(pkg.Members))
	for i := range pkg.Members {
		gotPaths[i] = pkg.Members[i].Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("member paths = %v, want %v", gotPaths, wantPaths)
	}
}

func TestCollectRejectsRequiredMissingAndSymlinkParentEscape(t *testing.T) {
	source, base := repository(t)
	spec := runSpec(base, false)
	spec.ExpectedOutputs = []executioncontract.ExpectedOutput{{Name: "evidence", Kind: "evidence", Root: executioncontract.RootArtifact, Path: "report.json", Required: true, Sensitivity: executioncontract.SensitivityInternal}}
	layout, err := Prepare(t.Context(), filepath.Join(t.TempDir(), "runs"), source, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Collect(t.Context(), layout, spec, "test"); err == nil || !strings.Contains(err.Error(), "declared output") {
		t.Fatalf("missing required output error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.Artifact, "report.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.ExpectedOutputs[0].MediaTypes = []string{"image/png"}
	if _, _, err := Collect(t.Context(), layout, spec, "test"); err == nil || !strings.Contains(err.Error(), "forbidden media type") {
		t.Fatalf("media policy error = %v", err)
	}
	if err := os.Remove(filepath.Join(layout.Artifact, "report.json")); err != nil {
		t.Fatal(err)
	}
	spec.ExpectedOutputs[0].MediaTypes = nil

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(layout.Artifact, "pivot")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "report.json"), []byte("escape"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.ExpectedOutputs[0].Path = "pivot/report.json"
	if _, _, err := Collect(t.Context(), layout, spec, "test"); err == nil || !strings.Contains(err.Error(), "escapes logical root") {
		t.Fatalf("symlink escape error = %v", err)
	}
}

func repository(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "base")
	sha, err := gitexec.Output(t.Context(), gitexec.Options{Dir: dir}, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return dir, sha
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := gitexec.Run(context.Background(), gitexec.Options{Dir: dir}, args...); err != nil {
		t.Fatal(err)
	}
}

func runSpec(base string, memory bool) executioncontract.RunSpec {
	return executioncontract.RunSpec{
		Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run-test", EffectID: "effect-test", IdempotencyKey: "intent-test",
		Fence: executioncontract.GenerationFence{TaskID: "task", TaskGeneration: 1, WorkflowID: "ship", WorkflowGeneration: 1, StepID: "implement"},
		Role:  "implementation", Provider: executioncontract.ProviderIntent{Provider: "claude", Model: "sonnet"}, Prompt: executioncontract.Prompt{Text: "work"}, Deadline: time.Now().Add(time.Hour),
		Options:   executioncontract.ExecutionOptions{SeedWorkingMemory: memory},
		Workspace: executioncontract.Workspace{RepositoryID: "repo", BaseSHA: base, BaseRef: "refs/heads/main", Roots: []executioncontract.LogicalRoot{executioncontract.RootWorktree, executioncontract.RootArtifact, executioncontract.RootSidecar, executioncontract.RootWorkingMemory}},
	}
}
