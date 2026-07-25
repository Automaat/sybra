package workflow

import (
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evidence"
)

// fakeEvidenceRecorder is an in-memory workflow.EvidenceRecorder. Append
// mirrors evidence.Store's upsert-by-criterion semantics (see
// internal/evidence/store.go) so tests exercising replay/staleness behave
// the same as the real recorder.
type fakeEvidenceRecorder struct {
	mu    sync.Mutex
	store map[string]evidence.CompletionEvidence
}

func newFakeEvidenceRecorder() *fakeEvidenceRecorder {
	return &fakeEvidenceRecorder{store: map[string]evidence.CompletionEvidence{}}
}

func (f *fakeEvidenceRecorder) AppendCriterion(taskID string, entry evidence.CriterionEvidence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ce := f.store[taskID]
	ce.TaskID = taskID
	replaced := false
	for i := range ce.Criteria {
		if ce.Criteria[i].Criterion == entry.Criterion {
			ce.Criteria[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		ce.Criteria = append(ce.Criteria, entry)
	}
	f.store[taskID] = ce
	return nil
}

func (f *fakeEvidenceRecorder) Evidence(taskID string) (evidence.CompletionEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.store[taskID], nil
}

// Set overwrites the whole CompletionEvidence for a task — used by tests that
// want to construct a specific evidence set directly rather than replaying
// individual Append calls.
func (f *fakeEvidenceRecorder) Set(taskID string, ce evidence.CompletionEvidence) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[taskID] = ce
}

func newRequireEvidenceStep() *Step { return &Step{ID: "require_evidence", Type: StepRequireEvidence} }

func newRequireEvidenceEngine(t *testing.T, wt string) (*Engine, *memTasks, *fakeEvidenceRecorder) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wt, ok: true})
	rec := newFakeEvidenceRecorder()
	engine.SetEvidenceRecorder(rec)
	engine.SetEvidenceConfig(config.EvidenceConfig{Enabled: true})
	return engine, engine.tasks.(*memTasks), rec
}

func gitOutputForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func headSHAForTest(t *testing.T, dir string) string {
	t.Helper()
	return gitOutputForTest(t, dir, "rev-parse", "HEAD")
}

func priorCommitSHAForTest(t *testing.T, dir string) string {
	t.Helper()
	return gitOutputForTest(t, dir, "rev-parse", "HEAD~1")
}

func TestExecRequireEvidence_DisabledSkips(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	engine.SetEvidenceConfig(config.EvidenceConfig{Enabled: false})
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{{Criterion: "verify_checks"}}})
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: evidence gate disabled" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
}

func TestExecRequireEvidence_NoBaselineSkips(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	engine, tasks, _ := newRequireEvidenceEngine(t, wt)
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "skipped: no evidence baseline recorded" {
		t.Errorf("Output = %q, want skip", out.Output)
	}
	if tasks.tasks["t1"].Status == "human-required" {
		t.Errorf("task flipped to human-required on an absent baseline")
	}
}

func TestExecRequireEvidence_CompleteAndFreshLands(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	engine.SetCheckConfigGetter(&fakeCheckGetter{cmds: []string{"true"}})
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionVerifyChecks, ExitStatus: 0, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "complete" {
		t.Errorf("Output = %q, want complete", out.Output)
	}
	if tasks.tasks["t1"].Status == "human-required" {
		t.Errorf("task flipped to human-required despite complete, fresh evidence")
	}
}

func TestExecRequireEvidence_MissingCriterionBlocks(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	engine.SetCheckConfigGetter(&fakeCheckGetter{cmds: []string{"true"}})
	// verify_checks omitted entirely, even though a verify suite is configured.
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(out.Output, "blocked: ") {
		t.Fatalf("Output = %q, want a blocked: prefix", out.Output)
	}
	if tasks.tasks["t1"].Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", tasks.tasks["t1"].Status)
	}
	if tasks.tasks["t1"].Blocker.Kind != blocker.KindOperatorDecision {
		t.Errorf("Blocker.Kind = %q, want %q", tasks.tasks["t1"].Blocker.Kind, blocker.KindOperatorDecision)
	}
	if !strings.Contains(tasks.reasons["t1"], evidenceCriterionVerifyChecks+": missing") {
		t.Errorf("reason = %q, want it to name the missing criterion", tasks.reasons["t1"])
	}
}

func TestExecRequireEvidence_FailedCriterionBlocks(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 1, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tasks.tasks["t1"].Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", tasks.tasks["t1"].Status)
	}
	if !strings.Contains(out.Output, "failed (exit 1)") {
		t.Errorf("Output = %q, want it to report the failed exit status", out.Output)
	}
}

// TestExecRequireEvidence_StaleAfterHEADMutationBlocks proves mutating code
// after evidence was collected (a new commit landing after the check ran)
// prevents landing — the evidence's FinalRev no longer matches current HEAD.
func TestExecRequireEvidence_StaleAfterHEADMutationBlocks(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	staleRev := priorCommitSHAForTest(t, wt) // HEAD~1: a rev older than the current worktree HEAD
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: staleRev},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: staleRev},
	}})
	tasks.Put(TaskInfo{ID: "t1"})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tasks.tasks["t1"].Status != "human-required" {
		t.Fatalf("Status = %q, want human-required", tasks.tasks["t1"].Status)
	}
	if !strings.Contains(out.Output, "stale") {
		t.Errorf("Output = %q, want it to report staleness", out.Output)
	}
}

// TestExecRequireEvidence_ReplayedDuplicateEvidenceStillBlocks proves that
// replaying (re-appending) a stale entry does not help it pass: Append
// upserts by criterion name, so the replay just re-confirms the same stale
// FinalRev rather than accumulating a second, fresher-looking entry.
func TestExecRequireEvidence_ReplayedDuplicateEvidenceStillBlocks(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	staleRev := priorCommitSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)

	entry := evidence.CriterionEvidence{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: staleRev}
	if err := rec.AppendCriterion("t1", entry); err != nil {
		t.Fatalf("AppendCriterion: %v", err)
	}
	// Replay the identical stale entry a second time.
	if err := rec.AppendCriterion("t1", entry); err != nil {
		t.Fatalf("AppendCriterion (replay): %v", err)
	}
	if err := rec.AppendCriterion("t1", evidence.CriterionEvidence{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: staleRev}); err != nil {
		t.Fatalf("AppendCriterion: %v", err)
	}
	tasks.Put(TaskInfo{ID: "t1"})

	ce, _ := rec.Evidence("t1")
	if len(ce.Criteria) != 2 {
		t.Fatalf("Criteria = %d entries after replay, want 2 (upsert, not append)", len(ce.Criteria))
	}

	if _, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tasks.tasks["t1"].Status != "human-required" {
		t.Fatalf("Status = %q, want human-required — a replayed stale entry must not land", tasks.tasks["t1"].Status)
	}
}

func TestExecRequireEvidence_ReviewedTaskRequiresReviewCriterion(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t1", Reviewed: true})

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1", Reviewed: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, evidenceCriterionReview+": missing") {
		t.Errorf("Output = %q, want it to require the review criterion for a reviewed task", out.Output)
	}
}

// TestExecRequireEvidence_TestedTaskRequiresTestRunnerCriterion proves the
// test_runner requirement is derived from the task's durable AgentRuns history
// (which survives the workflow handoff), not the fresh current Execution's
// StepCounts — so a task that went through testing but has no test-runner
// evidence still blocks at the require_evidence gate.
func TestExecRequireEvidence_TestedTaskRequiresTestRunnerCriterion(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
		// test_runner evidence omitted despite a prior test-runner run.
	}})
	tested := TaskInfo{ID: "t1", AgentRuns: []AgentRunInfo{{Role: testRunnerRole}}}
	tasks.Put(tested)

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), tested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, evidenceCriterionTestRunner+": missing") {
		t.Errorf("Output = %q, want it to require the test_runner criterion for a tested task", out.Output)
	}
	if tasks.tasks["t1"].Status != "human-required" {
		t.Errorf("Status = %q, want human-required", tasks.tasks["t1"].Status)
	}
}

// TestExecRequireEvidence_UntestedTaskSkipsTestRunnerCriterion proves a task
// that never went through a test-runner run is not held to the test_runner
// criterion — otherwise every un-tested task would be blocked on evidence no
// producer ever had a chance to record.
func TestExecRequireEvidence_UntestedTaskSkipsTestRunnerCriterion(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
	}})
	untested := TaskInfo{ID: "t1"}
	tasks.Put(untested)

	out, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), untested)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "complete" {
		t.Errorf("Output = %q, want complete for an untested task", out.Output)
	}
}

func TestExecRequireEvidence_FiresDecisionHook(t *testing.T) {
	t.Parallel()
	wt := makeGitRepo(t, true)
	head := headSHAForTest(t, wt)
	engine, tasks, rec := newRequireEvidenceEngine(t, wt)
	rec.Set("t1", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 0, FinalRev: head},
		{Criterion: evidenceCriterionDetectTampering, ExitStatus: 0, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t1"})

	var got []EvidenceDecision
	engine.SetEvidenceDecisionHook(func(_ TaskInfo, d EvidenceDecision) {
		got = append(got, d)
	})

	if _, err := engine.execRequireEvidence("t1", newRequireEvidenceStep(), TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Outcome != "verified" {
		t.Fatalf("decisions = %+v, want exactly one verified decision", got)
	}

	// Now break it and confirm the hook fires "blocked".
	rec.Set("t2", evidence.CompletionEvidence{Criteria: []evidence.CriterionEvidence{
		{Criterion: evidenceCriterionVerifyCommits, ExitStatus: 1, FinalRev: head},
	}})
	tasks.Put(TaskInfo{ID: "t2"})
	if _, err := engine.execRequireEvidence("t2", newRequireEvidenceStep(), TaskInfo{ID: "t2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[1].Outcome != "blocked" || got[1].Reason == "" {
		t.Fatalf("decisions = %+v, want a second blocked decision with a reason", got)
	}
}
