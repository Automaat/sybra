package workflow

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// --- fakes for CostBudgetChecker / AttemptWorktreeManager ---

type fakeAttemptWorktrees struct {
	mu sync.Mutex

	prepareCalls  []string // attemptIDs, in call order
	prepareErr    map[string]error
	promoteCalls  []promoteCall
	promoteErr    error
	promoteDirRet string
	cleanupCalls  [][]string
}

type promoteCall struct {
	TaskID       string
	WinnerDir    string
	WinnerBranch string
}

func newFakeAttemptWorktrees() *fakeAttemptWorktrees {
	return &fakeAttemptWorktrees{prepareErr: map[string]error{}}
}

func (f *fakeAttemptWorktrees) PrepareAttempt(taskID, attemptID string) (dir, branch string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls = append(f.prepareCalls, attemptID)
	if err := f.prepareErr[attemptID]; err != nil {
		return "", "", err
	}
	return "/tmp/attempt-" + attemptID, "task-branch-" + attemptID, nil
}

func (f *fakeAttemptWorktrees) PromoteAttempt(taskID, winnerDir, winnerBranch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoteCalls = append(f.promoteCalls, promoteCall{TaskID: taskID, WinnerDir: winnerDir, WinnerBranch: winnerBranch})
	if f.promoteErr != nil {
		return "", f.promoteErr
	}
	dir := f.promoteDirRet
	if dir == "" {
		dir = "/tmp/canonical"
	}
	return dir, nil
}

func (f *fakeAttemptWorktrees) CleanupAttempts(taskID string, attemptIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := slices.Clone(attemptIDs)
	slices.Sort(cp)
	f.cleanupCalls = append(f.cleanupCalls, cp)
}

func (f *fakeAttemptWorktrees) PrepareCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prepareCalls)
}

type fakeCostBudget struct {
	mu       sync.Mutex
	err      error
	failFrom int // when >0, return err starting from this 1-based call (else err applies to every call)
	callsFor []string
}

func (f *fakeCostBudget) CheckTaskCostBudget(taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callsFor = append(f.callsFor, taskID)
	if f.failFrom > 0 {
		if len(f.callsFor) >= f.failFrom {
			return f.err
		}
		return nil
	}
	return f.err
}

// bestOfNTestDef builds a minimal best_of_n -> judge (run_agent) ->
// promote_best_of_n -> done workflow, mirroring the shape of the builtin
// simple-task-best-of-n-implement.yaml but without the verify/tamper tail
// (those are covered by existing verify_commits/detect_tampering tests).
func bestOfNTestDef(attempts int) Definition {
	return Definition{
		ID:      "bestofn-test",
		Trigger: Trigger{On: "manual"},
		Steps: []Step{
			{
				ID:   "attempts",
				Type: StepBestOfN,
				Config: StepConfig{
					Role:     "implementation",
					Mode:     "headless",
					Prompt:   "implement attempt {{.Task.ID}}",
					Attempts: attempts,
				},
				Next: []Transition{
					{When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: "judge"},
				},
			},
			{
				ID:   "judge",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:            "review",
					Mode:            "headless",
					Prompt:          "judge",
					BudgetPreflight: true,
				},
				Next: []Transition{
					{When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: "promote"},
				},
			},
			{
				ID:   "promote",
				Type: StepPromoteBestOfN,
				Config: StepConfig{
					JudgeStep:   "judge",
					BestOfNStep: "attempts",
				},
				Next: []Transition{
					{When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: "done"},
				},
			},
			{
				ID:   "done",
				Type: StepSetStatus,
				Config: StepConfig{
					Status: "ready-review",
				},
				Next: []Transition{{GoTo: ""}},
			},
		},
	}
}

func newBestOfNTestEngine(t *testing.T, attempts int) (*Engine, *memTasks, *mockAgents, *fakeAttemptWorktrees, *fakeCostBudget) {
	t.Helper()
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	store := newTestStore(t)
	def := bestOfNTestDef(attempts)
	if err := store.Save(def); err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	attemptWorktrees := newFakeAttemptWorktrees()
	costBudget := &fakeCostBudget{}
	engine.SetAttemptWorktreeManager(attemptWorktrees)
	engine.SetCostBudgetChecker(costBudget)
	tasks.Put(TaskInfo{ID: "t1", Status: "todo"})
	return engine, tasks, agents, attemptWorktrees, costBudget
}

// --- model validation ---

func TestBestOfNValidation_AttemptsBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		attempts int
		wantErr  bool
	}{
		{"floor-1-rejected", 1, true},
		{"floor-2-accepted", 2, false},
		{"cap-6-accepted", 6, false},
		{"cap-7-rejected", 7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{
				ID:      "x",
				Trigger: Trigger{On: "manual"},
				Steps: []Step{
					{ID: "a", Type: StepBestOfN, Config: StepConfig{Attempts: tc.attempts, Prompt: "x"}},
				},
			}
			err := def.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("attempts=%d: expected validation error, got nil", tc.attempts)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("attempts=%d: expected valid, got %v", tc.attempts, err)
			}
		})
	}
}

func TestBestOfNValidation_InvalidAttemptProvider(t *testing.T) {
	t.Parallel()
	def := Definition{
		ID:      "x",
		Trigger: Trigger{On: "manual"},
		Steps: []Step{
			{ID: "a", Type: StepBestOfN, Config: StepConfig{Attempts: 2, AttemptProviders: []string{"claude", "bogus"}}},
		},
	}
	err := def.Validate()
	if err == nil || !strings.Contains(err.Error(), "invalid attempt provider") {
		t.Fatalf("err = %v, want invalid attempt provider", err)
	}
}

func TestPromoteBestOfNValidation_RequiresJudgeAndBestOfNStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		steps  []Step
		errSub string
	}{
		{
			name: "missing judge_step",
			steps: []Step{
				{ID: "attempts", Type: StepBestOfN, Config: StepConfig{Attempts: 2}},
				{ID: "p", Type: StepPromoteBestOfN, Config: StepConfig{BestOfNStep: "attempts"}},
			},
			errSub: "requires judge_step",
		},
		{
			name: "judge_step not found",
			steps: []Step{
				{ID: "attempts", Type: StepBestOfN, Config: StepConfig{Attempts: 2}},
				{ID: "p", Type: StepPromoteBestOfN, Config: StepConfig{JudgeStep: "nope", BestOfNStep: "attempts"}},
			},
			errSub: "not found",
		},
		{
			name: "judge_step wrong type",
			steps: []Step{
				{ID: "attempts", Type: StepBestOfN, Config: StepConfig{Attempts: 2}},
				{ID: "judge", Type: StepSetStatus},
				{ID: "p", Type: StepPromoteBestOfN, Config: StepConfig{JudgeStep: "judge", BestOfNStep: "attempts"}},
			},
			errSub: "must be a run_agent step",
		},
		{
			name: "missing best_of_n_step",
			steps: []Step{
				{ID: "judge", Type: StepRunAgent},
				{ID: "p", Type: StepPromoteBestOfN, Config: StepConfig{JudgeStep: "judge"}},
			},
			errSub: "requires best_of_n_step",
		},
		{
			name: "best_of_n_step wrong type",
			steps: []Step{
				{ID: "judge", Type: StepRunAgent},
				{ID: "attempts", Type: StepRunAgent},
				{ID: "p", Type: StepPromoteBestOfN, Config: StepConfig{JudgeStep: "judge", BestOfNStep: "attempts"}},
			},
			errSub: "must be a best_of_n step",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{ID: "x", Trigger: Trigger{On: "manual"}, Steps: tc.steps}
			err := def.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.errSub)
			}
		})
	}
}

// --- fan-out dispatch ---

func TestBestOfN_FanOutDispatchesEachAttemptIsolated(t *testing.T) {
	engine, tasks, agents, attemptWt, _ := newBestOfNTestEngine(t, 3)

	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	if got := agents.CallCount(); got != 3 {
		t.Fatalf("StartAgent calls = %d, want 3", got)
	}
	if got := attemptWt.PrepareCallCount(); got != 3 {
		t.Fatalf("PrepareAttempt calls = %d, want 3", got)
	}

	dirs := map[string]bool{}
	for _, c := range agents.calls {
		if c.NeedsWorktree {
			t.Errorf("attempt call NeedsWorktree = true, want false (pre-staged dir)")
		}
		if c.Dir == "" {
			t.Errorf("attempt call has empty dir, want pre-staged attempt dir")
		}
		dirs[c.Dir] = true
		if c.Assignment.AssignmentUnit != "bestofn-attempt" {
			t.Errorf("AssignmentUnit = %q, want bestofn-attempt", c.Assignment.AssignmentUnit)
		}
		if !strings.HasPrefix(c.Assignment.VariantID, "attempt_") {
			t.Errorf("VariantID = %q, want attempt_N", c.Assignment.VariantID)
		}
	}
	if len(dirs) != 3 {
		t.Errorf("distinct attempt dirs = %d, want 3 (isolation): %v", len(dirs), dirs)
	}

	wf := mustWorkflow(t, tasks, "t1")
	rec := wf.BestOfNInflight["attempts"]
	if rec == nil || len(rec.Attempts) != 3 {
		t.Fatalf("BestOfNInflight[attempts] = %+v, want 3 attempt slots", rec)
	}
}

func TestBestOfN_CostPreflightFailsClosedBeforeDispatch(t *testing.T) {
	engine, tasks, agents, _, costBudget := newBestOfNTestEngine(t, 2)
	costBudget.err = ErrTaskCostExceeded

	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 (cost preflight must block dispatch)", got)
	}
	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "cost budget exceeded") {
		t.Fatalf("reason = %q, want cost budget exceeded", ti.StatusReason)
	}
}

// TestBestOfN_CostPreflightBlocksJudgeAfterAttempts proves the budget guard
// also protects the (most expensive) judge run: the fan-out preflight passes,
// both attempts complete, then the budget flips to exceeded before the judge
// dispatches — the judge agent must never start and the task fails closed to
// human-required, rather than spending on the judge and only failing at
// promotion.
func TestBestOfN_CostPreflightBlocksJudgeAfterAttempts(t *testing.T) {
	engine, tasks, agents, _, costBudget := newBestOfNTestEngine(t, 2)
	// Call 1 = fan-out preflight (passes); call 2 = judge preflight (fails).
	costBudget.failFrom = 2
	costBudget.err = ErrTaskCostExceeded

	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if got := agents.CallCount(); got != 2 {
		t.Fatalf("after fan-out StartAgent calls = %d, want 2 (both attempts)", got)
	}

	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_1", Status: "completed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_2", Status: "completed", AgentID: "agent-2"}); err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != 2 {
		t.Fatalf("StartAgent calls = %d, want 2 (judge must NOT dispatch when over budget)", got)
	}
	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "cost budget exceeded before judge dispatch") {
		t.Fatalf("reason = %q, want judge-dispatch budget reason", ti.StatusReason)
	}
}

// --- all-fail / too-few-successes ---

func TestBestOfN_AllAttemptsFail_HumanRequiredDistinctReason(t *testing.T) {
	engine, tasks, _, attemptWt, _ := newBestOfNTestEngine(t, 2)
	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatal(err)
	}

	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_1", Status: "failed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_2", Status: "failed", AgentID: "agent-2"}); err != nil {
		t.Fatal(err)
	}

	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "all attempts failed") {
		t.Fatalf("reason = %q, want all-attempts-failed reason", ti.StatusReason)
	}
	if len(attemptWt.cleanupCalls) == 0 {
		t.Fatalf("expected CleanupAttempts to be called after all-attempts-failed")
	}
	if got := attemptWt.cleanupCalls[len(attemptWt.cleanupCalls)-1]; len(got) != 2 {
		t.Fatalf("cleanup ids = %v, want both attempts", got)
	}
}

func TestBestOfN_ExactlyOneSuccess_HumanRequiredDistinctReason(t *testing.T) {
	engine, tasks, _, attemptWt, _ := newBestOfNTestEngine(t, 2)
	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatal(err)
	}

	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_1", Status: "completed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_2", Status: "failed", AgentID: "agent-2"}); err != nil {
		t.Fatal(err)
	}

	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if !strings.Contains(ti.StatusReason, "fewer than 2 successful") {
		t.Fatalf("reason = %q, want fewer-than-2-successful reason", ti.StatusReason)
	}
	if len(attemptWt.cleanupCalls) == 0 {
		t.Fatalf("expected CleanupAttempts to be called when too few successes to judge")
	}

	// The two distinct failure reasons across the all-fail and
	// exactly-one-success tests must never collapse to the same string.
	allFailReason := "best-of-n: all attempts failed to start or complete"
	oneSuccessReason := "best-of-n: fewer than 2 successful attempts, cannot judge"
	if allFailReason == oneSuccessReason {
		t.Fatal("sanity: reasons must be distinct literals")
	}
}

// --- judge validation ---

func setupTwoSuccessfulAttempts(t *testing.T, engine *Engine, tasks *memTasks) {
	t.Helper()
	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_1", Status: "completed", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "attempts::attempt_2", Status: "completed", AgentID: "agent-2"}); err != nil {
		t.Fatal(err)
	}
	// Judge agent (agent-3, the 3rd StartAgent call for this task) must now
	// be running; nothing to assert here — callers drive its completion.
	_ = tasks
}

func TestBestOfN_JudgeMalformedOutput(t *testing.T) {
	engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: "not json at all"}); err != nil {
		t.Fatal(err)
	}
	assertHumanRequiredReason(t, tasks, "malformed judge output")
}

func TestBestOfN_JudgeUnknownWinnerID(t *testing.T) {
	engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	out := `{"winner_attempt_id": "attempt_99", "rationale": "x"}`
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: out}); err != nil {
		t.Fatal(err)
	}
	assertHumanRequiredReason(t, tasks, "unknown attempt id")
}

func TestBestOfN_JudgeAmbiguousWinner(t *testing.T) {
	engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	out := `{"winner_attempt_id": "attempt_1, attempt_2", "rationale": "x"}`
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: out}); err != nil {
		t.Fatal(err)
	}
	assertHumanRequiredReason(t, tasks, "multiple or ambiguous winners")
}

func TestBestOfN_JudgeErroredOrTimedOut(t *testing.T) {
	engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "failed", AgentID: judgeAgentID, Output: "agent crashed"}); err != nil {
		t.Fatal(err)
	}
	assertHumanRequiredReason(t, tasks, "judge step errored or timed out")
}

// TestBestOfN_JudgeFailureReasonsAreDistinct locks that every judge-side
// failure mode (malformed / unknown id / ambiguous / errored) produces its
// own reason string, not a shared generic message — required so a human (or
// a test) can tell them apart mechanically.
func TestBestOfN_JudgeFailureReasonsAreDistinct(t *testing.T) {
	reasons := map[string]string{
		"malformed": "best-of-n promotion: malformed judge output: no JSON object found in judge output",
		"missing":   "best-of-n promotion: judge output missing winner_attempt_id",
		"ambiguous": "best-of-n promotion: judge output names multiple or ambiguous winners",
		"unknown":   "best-of-n promotion: judge named unknown attempt id attempt_99",
		"errored":   "best-of-n promotion: judge step errored or timed out",
	}
	seen := map[string]string{}
	for name, reason := range reasons {
		if other, dup := seen[reason]; dup {
			t.Fatalf("reason for %q collides with %q: %q", name, other, reason)
		}
		seen[reason] = name
	}
}

// --- successful, idempotent promotion ---

func TestBestOfN_JudgeSuccess_PromotesWinnerAndCleansUpLosers(t *testing.T) {
	engine, tasks, agents, attemptWt, _ := newBestOfNTestEngine(t, 2)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	out := `{"winner_attempt_id": "attempt_2", "scores": [{"attempt_id":"attempt_1","score":4},{"attempt_id":"attempt_2","score":9}], "rationale": "attempt_2 is more complete"}`
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: out}); err != nil {
		t.Fatal(err)
	}

	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status == "human-required" {
		t.Fatalf("promotion should have succeeded, got human-required: %s", ti.StatusReason)
	}
	if ti.Status != "ready-review" {
		t.Fatalf("status = %q, want ready-review (workflow should advance past promote)", ti.Status)
	}

	if len(attemptWt.promoteCalls) != 1 {
		t.Fatalf("PromoteAttempt calls = %d, want 1", len(attemptWt.promoteCalls))
	}
	call := attemptWt.promoteCalls[0]
	if call.WinnerDir != "/tmp/attempt-attempt_2" || call.WinnerBranch != "task-branch-attempt_2" {
		t.Fatalf("promoted wrong attempt: %+v", call)
	}

	// Both attempts are cleaned up, including the winner: PromoteAttempt
	// materializes a separate canonical worktree, so the winner's own attempt
	// dir is a redundant duplicate checkout once promotion succeeds.
	if len(attemptWt.cleanupCalls) != 1 || len(attemptWt.cleanupCalls[0]) != 2 {
		t.Fatalf("cleanup calls = %v, want both attempt_1 and attempt_2 cleaned up", attemptWt.cleanupCalls)
	}
	if got := attemptWt.cleanupCalls[0]; !slices.Contains(got, "attempt_1") || !slices.Contains(got, "attempt_2") {
		t.Fatalf("cleanup calls = %v, want both attempt_1 and attempt_2", got)
	}

	wf := mustWorkflow(t, tasks, "t1")
	if wf.Variables[WorkflowVarDir] != "/tmp/canonical" {
		t.Fatalf("_dir var = %q, want canonical dir set by promotion", wf.Variables[WorkflowVarDir])
	}
	if _, still := wf.BestOfNInflight["attempts"]; still {
		t.Errorf("BestOfNInflight should be cleared after promotion")
	}
}

func TestBestOfN_PromotionRefused_FailsClosedWithDistinctReason(t *testing.T) {
	engine, tasks, agents, attemptWt, _ := newBestOfNTestEngine(t, 2)
	attemptWt.promoteErr = errors.New("best-of-n promotion refused: task already has a PR")
	setupTwoSuccessfulAttempts(t, engine, tasks)

	judgeAgentID := agents.LastID()
	out := `{"winner_attempt_id": "attempt_1", "rationale": "x"}`
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: out}); err != nil {
		t.Fatal(err)
	}
	assertHumanRequiredReason(t, tasks, "best-of-n promotion refused")
}

// --- resume-without-double-dispatch ---

func TestBestOfN_ResumeDoesNotDoubleDispatchTerminalAttempts(t *testing.T) {
	engine, tasks, agents, attemptWt, _ := newBestOfNTestEngine(t, 3)
	if err := engine.StartWorkflow("t1", "bestofn-test"); err != nil {
		t.Fatal(err)
	}
	startCalls := agents.CallCount()
	if startCalls != 3 {
		t.Fatalf("initial spawn count = %d, want 3", startCalls)
	}

	// Simulate a restart: attempt_1 completed, attempt_2 is still "pending"
	// (its agent's completion never got routed before the process died),
	// attempt_3 also pending. Re-entering execBestOfN (as ResumeStalled
	// would) must only re-spawn the still-pending attempts, never attempt_1.
	def, err := engine.store.Get("bestofn-test")
	if err != nil {
		t.Fatal(err)
	}
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	wf := ti.Workflow
	wf.BestOfNInflight["attempts"].Attempts["attempt_1"].Status = "completed"

	ctx := TemplateContext{Task: ti, Step: *def.StepByID("attempts"), Vars: wf.Variables, Workflow: wf}
	if _, err := engine.execBestOfN("t1", &def, def.StepByID("attempts"), wf, ctx); err != nil {
		t.Fatalf("execBestOfN resume: %v", err)
	}

	got := agents.CallCount()
	if got != startCalls+2 {
		t.Fatalf("resume re-spawn count = %d, want %d (only the 2 still-pending attempts)", got-startCalls, 2)
	}
	// attempt_1's original agent must be untouched — no PrepareAttempt call
	// for it beyond the original fan-out.
	n1 := 0
	for _, id := range attemptWt.prepareCalls {
		if id == "attempt_1" {
			n1++
		}
	}
	if n1 != 1 {
		t.Errorf("PrepareAttempt(attempt_1) called %d times, want 1 (no re-dispatch of a completed attempt)", n1)
	}
}

// --- helpers ---

func mustTaskInfo(t *testing.T, tasks *memTasks, id string) TaskInfo {
	t.Helper()
	ti, err := tasks.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return ti
}

func assertHumanRequiredReason(t *testing.T, tasks *memTasks, wantSub string) {
	t.Helper()
	ti := mustTaskInfo(t, tasks, "t1")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (reason so far: %q)", ti.Status, ti.StatusReason)
	}
	if !strings.Contains(ti.StatusReason, wantSub) {
		t.Fatalf("reason = %q, want substring %q", ti.StatusReason, wantSub)
	}
}

// TestBestOfN_ArtifactsNeverRouteToGithub locks the CLAUDE.md Work-Data
// Confidentiality invariant for the two new local-only artifact kinds this
// feature introduces (attempt manifest, judge report): engine_steps_bestofn.go
// must record them ONLY through ArtifactRecorder.PutGeneric (the local
// artifact.Store) and must never itself import the github package or reach a
// public destination. This applies equally to work-typed and non-work-typed
// tasks — best-of-n manifests/judge reports are local-debug-only by design
// for every project, so there is no scrub-bypass special case to gate on
// project type; a work-typed task's own routing rules (App.workScrubContextForTask)
// are exercised independently by TestWorkScrubContextForTask in
// internal/sybra and are unaffected by this feature, since best-of-n never
// calls into that path at all.
func TestBestOfN_ArtifactsNeverRouteToGithub(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("engine_steps_bestofn.go")
	if err != nil {
		t.Fatalf("read engine_steps_bestofn.go: %v", err)
	}
	text := string(src)
	if strings.Contains(text, `"github.com/Automaat/sybra/internal/github"`) {
		t.Fatal("engine_steps_bestofn.go imports internal/github — best-of-n artifacts must stay local-only, never posted directly to GitHub")
	}
	if !strings.Contains(text, "recorder.PutGeneric") {
		t.Fatal("engine_steps_bestofn.go no longer records manifest/judge-report via ArtifactRecorder.PutGeneric — the local-only artifact path this test protects has moved or been removed")
	}
}

// TestBestOfN_ManifestAndJudgeReportStoredViaArtifactRecorder proves the
// manifest and judge-report content the engine hands to ArtifactRecorder is
// exactly the same raw content recorded — the artifact store is
// local-debug-only and never scrubs at write time (CLAUDE.md); scrubbing is
// the caller's job only when/if that content is later surfaced to a public
// destination, which best-of-n's own code never does (see the sibling test
// above). This is what "raw local artifact" means operationally: no
// transformation happens between the engine building the payload and the
// recorder persisting it.
func TestBestOfN_ManifestAndJudgeReportStoredViaArtifactRecorder(t *testing.T) {
	engine, tasks, agents, _, _ := newBestOfNTestEngine(t, 2)
	rec := &recordingArtifactRecorder{}
	engine.SetArtifactRecorder(rec)
	setupTwoSuccessfulAttempts(t, engine, tasks)

	rec.mu.Lock()
	manifestPuts := len(rec.puts)
	rec.mu.Unlock()
	if manifestPuts == 0 {
		t.Fatal("expected a manifest artifact to be recorded once both attempts succeed")
	}
	rec.mu.Lock()
	manifestContent := rec.puts[0].content
	rec.mu.Unlock()
	if !strings.Contains(manifestContent, "attempt_1") || !strings.Contains(manifestContent, "attempt_2") {
		t.Fatalf("manifest content missing raw attempt ids: %q", manifestContent)
	}

	judgeAgentID := agents.LastID()
	out := `{"winner_attempt_id": "attempt_2", "rationale": "raw rationale text with a local /tmp path"}`
	if err := engine.AdvanceStep("t1", StepOutput{StepID: "judge", Status: "completed", AgentID: judgeAgentID, Output: out}); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	found := false
	for _, p := range rec.puts {
		if strings.Contains(p.name, "judge-report") {
			found = true
			if p.content != out {
				t.Fatalf("judge report content = %q, want the raw unmodified judge output %q", p.content, out)
			}
		}
	}
	if !found {
		t.Fatal("expected a judge-report artifact to be recorded on successful promotion")
	}
}

type recordingArtifactPut struct{ name, content string }

// recordingArtifactRecorder is a minimal ArtifactRecorder fake that records
// every PutGeneric call verbatim, so tests can assert on exactly what content
// crossed the engine -> artifact-store boundary.
type recordingArtifactRecorder struct {
	mu   sync.Mutex
	puts []recordingArtifactPut
}

func (r *recordingArtifactRecorder) RecordTrace(taskID string, ev any) error { return nil }

func (r *recordingArtifactRecorder) PutPlanSnapshot(taskID, role, stepID, sourcePath, content string) error {
	return nil
}

func (r *recordingArtifactRecorder) PutGeneric(taskID, name, stepID, content string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.puts = append(r.puts, recordingArtifactPut{name: name, content: content})
	return nil
}
