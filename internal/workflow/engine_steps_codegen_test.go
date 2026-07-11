package workflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCodegenGateStep() *Step { return &Step{ID: "codegen_gate", Type: StepCodegenGate} }

func newCodegenGateEngine(t *testing.T, wt string, cmds []string) (*Engine, *memTasks) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{codegen: cmds})
	return engine, engine.tasks.(*memTasks)
}

func codegenGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestExecCodegenGate_NoGetterSkips(t *testing.T) {
	t.Parallel()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: t.TempDir(), ok: true})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no check config getter" {
		t.Fatalf("Output = %q, want no-getter skip", out.Output)
	}
}

func TestExecCodegenGate_NoCommandsSkips(t *testing.T) {
	t.Parallel()
	engine, tasks := newCodegenGateEngine(t, t.TempDir(), nil)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no codegen commands configured" {
		t.Fatalf("Output = %q, want no-command skip", out.Output)
	}
}

func TestExecCodegenGate_NoWorktreeGetterSkips(t *testing.T) {
	t.Parallel()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetCheckConfigGetter(&fakeCheckGetter{codegen: []string{"true"}})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no worktree getter configured" {
		t.Fatalf("Output = %q, want no-worktree-getter skip", out.Output)
	}
}

func TestExecCodegenGate_NoWorktreeForTaskSkips(t *testing.T) {
	t.Parallel()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetCheckConfigGetter(&fakeCheckGetter{codegen: []string{"true"}})
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no worktree for task" {
		t.Fatalf("Output = %q, want no-worktree skip", out.Output)
	}
}

func TestExecCodegenGate_CleanTreeNoOp(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{"true"})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "clean" {
		t.Fatalf("Output = %q, want clean", out.Output)
	}
	if got := codegenGitOutput(t, wt, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("commit count = %s, want 1", got)
	}
	if got := codegenGitOutput(t, wt, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q, want clean tree", got)
	}
	if len(rec.puts) != 1 || rec.puts[0].name != "codegen-gate.json" {
		t.Fatalf("artifacts = %+v, want one codegen-gate.json artifact", rec.puts)
	}
	if !strings.Contains(rec.puts[0].content, `"committed": false`) {
		t.Fatalf("artifact content = %q, want committed=false", rec.puts[0].content)
	}
}

func TestExecCodegenGate_DriftCommits(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{"printf 'generated\\n' > generated.txt"})
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "committed" {
		t.Fatalf("Output = %q, want committed", out.Output)
	}
	if got := codegenGitOutput(t, wt, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("commit count = %s, want 2", got)
	}
	if got := codegenGitOutput(t, wt, "log", "-1", "--pretty=%s"); got != "chore(codegen): apply generated changes" {
		t.Fatalf("last subject = %q, want codegen checkpoint subject", got)
	}
	if got := codegenGitOutput(t, wt, "status", "--porcelain"); got != "" {
		t.Fatalf("status = %q, want clean tree after checkpoint", got)
	}
	if len(rec.puts) != 1 || !strings.Contains(rec.puts[0].content, `"committed": true`) {
		t.Fatalf("artifact content = %+v, want committed=true artifact", rec.puts)
	}
}

func TestExecCodegenGate_MissingToolchainFlagsHumanRequired(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{`echo "wails3: command not found" >&2; exit 1`})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "configured toolchain is missing from PATH") {
		t.Fatalf("status reason = %q, want missing toolchain detail", ti.StatusReason)
	}
}

func TestExecCodegenGate_HardFailureFlagsHumanRequired(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{`echo "boom" >&2; exit 1`})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "codegen gate failed while running") {
		t.Fatalf("status reason = %q, want codegen failure detail", ti.StatusReason)
	}
}

func TestExecCodegenGate_TimeoutFlagsHumanRequired(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{"sleep 1"})
	engine.SetVerifyTimeout(100 * time.Millisecond)
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "exceeded the time budget") {
		t.Fatalf("status reason = %q, want timeout detail", ti.StatusReason)
	}
}

func TestExecCodegenGate_CommitFailureFlagsHumanRequired(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	if err := os.WriteFile(filepath.Join(wt, ".git", "index.lock"), []byte("locked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, tasks := newCodegenGateEngine(t, wt, []string{"printf 'generated\\n' > generated.txt"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Fatalf("Output = %q, want flagged", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "codegen gate could not checkpoint generated changes") {
		t.Fatalf("status reason = %q, want checkpoint failure detail", ti.StatusReason)
	}
}

func TestExecCodegenGate_ContextCanceledSkips(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newCodegenGateEngine(t, wt, []string{"sleep 5"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	engine.ctx = ctx

	out, err := engine.execCodegenGate("t1", newCodegenGateStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: context canceled" {
		t.Fatalf("Output = %q, want canceled skip", out.Output)
	}
}
