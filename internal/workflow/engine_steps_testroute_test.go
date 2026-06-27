package workflow

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExtractTestVerdict(t *testing.T) {
	t.Parallel()

	longPrefix := strings.Repeat("exercised an edge case and it held. ", 100) // > 2000 bytes

	cases := []struct {
		name   string
		output string
		want   string
	}{
		// --- plain-text marker path (claude) ---
		{"pass_final_line", "ran the app\nTEST_VERDICT: PASS", "PASS"},
		{"fail_final_line", "found a defect\nTEST_VERDICT: FAIL", "FAIL"},
		{"pass_after_long_summary", longPrefix + "\nTEST_VERDICT: PASS", "PASS"},
		{"fail_after_long_summary", longPrefix + "\nTEST_VERDICT: FAIL", "FAIL"},
		{"no_marker", "I ran some checks but never concluded", ""},
		{"empty", "", ""},
		{"last_marker_wins", "TEST_VERDICT: FAIL\n...then fixed and re-ran...\nTEST_VERDICT: PASS", "PASS"},
		{"trailing_whitespace", "TEST_VERDICT: PASS   \n", "PASS"},
		// Incidental mentions in prose must NOT be read as a verdict (exact-line
		// match, not substring) — else an agent quoting the contract ships broken.
		{"contract_quote_ignored", "Remember: the final line must be exactly TEST_VERDICT: PASS\nbut I never actually ran the app", ""},
		{"inline_prose_after_marker_ignored", "TEST_VERDICT: PASS (could not break it)", ""},

		// --- JSON object path (codex --output-schema) ---
		{"json_pass", `{"verdict":"PASS"}`, "PASS"},
		{"json_fail", `{"verdict":"FAIL"}`, "FAIL"},
		{"json_pass_lowercase", `{"verdict":"pass"}`, "PASS"},
		{"json_fail_lowercase", `{"verdict":"fail"}`, "FAIL"},
		{"json_with_summary", `{"verdict":"PASS","summary":"could not break it"}`, "PASS"},
		{"json_invalid_verdict", `{"verdict":"maybe"}`, ""},
		{"json_malformed_object", `{"oops":`, ""},
		// A JSON object that contains a marker-shaped substring must NOT route via
		// the marker — JSON authority is absolute for object-shaped output.
		{"json_marker_substring_no_fallthrough", `{"note":"saw TEST_VERDICT: FAIL in logs"}`, ""},
		// BOM + leading whitespace stripping.
		{"json_bom_prefix", "\xef\xbb\xbf{\"verdict\":\"PASS\"}", "PASS"},
		{"json_leading_whitespace", "  \n{\"verdict\":\"PASS\"}", "PASS"},
		// Non-object shapes fall through to the marker scan (safe direction).
		{"json_array_not_object", `[{"verdict":"PASS"}]`, ""},
		{"fenced_json_no_marker", "```json\n{\"verdict\":\"PASS\"}\n```", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := extractTestVerdict(tc.output); got != tc.want {
				t.Errorf("extractTestVerdict(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

func TestContainsFixSuggestions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "change_x_to_y",
			output: "## Test Failures\n\nCode evidence: route.go: change AgentRun.Verdict to VerdictRendered\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "switch_x_to_y",
			output: "## Test Failures\n\nCode evidence: route.go: switch AgentRun.Verdict to VerdictRendered\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "replace_x_with_y",
			output: "## Test Failures\n\nCode evidence: route.go: replace AgentRun.Verdict with VerdictRendered\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "use_x_instead_of_y",
			output: "## Test Failures\n\nCode evidence: route.go: use VerdictRendered instead of AgentRun.Verdict\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "rename_x_to_y",
			output: "## Test Failures\n\nCode evidence: route.go: rename Verdict to VerdictRendered\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "recommendation_phrase",
			output: "## Test Failures\n\nI recommend switching to VerdictRendered instead\nTEST_VERDICT: FAIL",
			want:   true,
		},
		{
			name:   "actual_output_not_advice",
			output: "## Test Failures\n\nActual output: \"use --new instead of --old\"\nExpected: command succeeds",
			want:   false,
		},
		{
			name:   "fenced_output_not_advice",
			output: "## Test Failures\n\nActual output:\n```text\nyou should use --new instead of --old\n```",
			want:   false,
		},
		{
			name:   "quoted_code_evidence_not_advice",
			output: "## Test Failures\n\nCode evidence: cli.go: `fmt.Println(\"use --new instead of --old\")`",
			want:   false,
		},
		{
			name:   "symptom_only",
			output: "## Test Failures\n\nCommand: curl /status\nActual: HTTP 500\nExpected: HTTP 200",
			want:   false,
		},
		{
			name:   "you_should_see_is_not_a_fix",
			output: "## Test Failures\n\nYou should see a 200 response but instead got 500",
			want:   false,
		},
		{
			name:   "fix_language_before_failures_section_ignored",
			output: "I recommend changing the handler.\n\n## Test Failures\n\nCommand returned exit code 1.",
			want:   false,
		},
		{
			name:   "no_failures_section",
			output: "I recommend changing the handler.\nTEST_VERDICT: FAIL",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := containsFixSuggestions(tc.output); got != tc.want {
				t.Errorf("containsFixSuggestions(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

func TestContainsFixSuggestionsInCurrentTestReport_MissingBodyStartIgnoresStaleBody(t *testing.T) {
	t.Parallel()

	body := "## Problem\nold task text\n\n## Test Failures\n\nI recommend changing the handler.\n"
	wf := &Execution{Variables: map[string]string{}}
	if containsFixSuggestionsInCurrentTestReport(`{"verdict":"FAIL"}`, body, wf, testVerdictSourceStep) {
		t.Fatal("missing body_start_len must not scan stale full task body")
	}
}

// makeTestEngine builds a minimal Engine for route_test_result unit tests.
func makeTestEngine(t *testing.T) (*Engine, *memTasks) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetTestingMaxAttempts(3)
	return engine, tasks
}

// runRouteTestResult calls execRouteTestResult with the given verdict and
// agent-run history seeded into the in-memory task store.
// cycleStart, when non-nil, simulates a human re-dispatch boundary: only runs
// at or after that time are counted toward the cap.
func runRouteTestResult(e *Engine, tasks *memTasks, taskID, verdict string, wfStartedAt time.Time, runs []AgentRunInfo, cycleStart *time.Time) (StepOutput, error) {
	tasks.Put(TaskInfo{
		ID:                    taskID,
		Status:                "testing",
		AgentRuns:             runs,
		TestingCycleStartedAt: cycleStart,
	})
	step := &Step{ID: "route_test"}
	wfExec := &Execution{
		WorkflowID: "testing-task",
		StartedAt:  wfStartedAt,
		Variables:  map[string]string{},
	}
	if verdict != "" {
		wfExec.Variables["step."+testVerdictSourceStep+".verdict"] = verdict
	}
	ti, _ := tasks.GetTask(taskID)
	return e.execRouteTestResult(taskID, step, wfExec, ti)
}

func makeTestingTaskEngine(t *testing.T) (*Engine, *memTasks, *mockAgents) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatal(err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	return engine, tasks, agents
}

func TestAdvanceStep_TestProtocolViolationRetriesRunTestFromBodyDelta(t *testing.T) {
	t.Parallel()
	engine, tasks, agents := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	body := initialBody + "\n\n### Failure\nCode evidence: route.go: switch AgentRun.Verdict to VerdictRendered\n"
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-proto",
		Status:    "testing",
		Body:      body,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-original", Role: testRunnerRole}},
	})

	err := engine.AdvanceStep("t-proto", StepOutput{
		StepID:   testVerdictSourceStep,
		Status:   "completed",
		Output:   `{"verdict":"FAIL"}`,
		AgentID:  "agent-original",
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != 1 {
		t.Fatalf("retry StartAgent calls = %d, want 1", got)
	}
	got, err := tasks.GetTask("t-proto")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "testing" {
		t.Errorf("status = %q, want testing", got.Status)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep != testVerdictSourceStep || got.Workflow.State != ExecWaiting {
		t.Fatalf("workflow = %+v, want waiting at run_test", got.Workflow)
	}
	if len(got.Workflow.StepHistory) != 1 {
		t.Fatalf("step history len = %d, want 1", len(got.Workflow.StepHistory))
	}
	rec := got.Workflow.StepHistory[0]
	if rec.Status != "failed" {
		t.Errorf("run_test record status = %q, want failed", rec.Status)
	}
	if !strings.Contains(rec.Output, "protocol violation") {
		t.Errorf("run_test output = %q, want protocol violation note", rec.Output)
	}
	if got.Workflow.Variables["step."+testVerdictSourceStep+"."+testVerdictTaintedKey] != "" {
		t.Errorf("taint var should be cleared for retry, got %q", got.Workflow.Variables["step."+testVerdictSourceStep+"."+testVerdictTaintedKey])
	}
	if got.Workflow.Variables["step."+testVerdictSourceStep+"."+testFailureBodyStartLenKey] != strconv.Itoa(len(body)) {
		t.Errorf("body start var = %q, want current body length", got.Workflow.Variables["step."+testVerdictSourceStep+"."+testFailureBodyStartLenKey])
	}
	if got.AgentRuns[0].ProtocolViolation != testProtocolFixSuggestions {
		t.Errorf("agent run protocol violation = %q, want %q", got.AgentRuns[0].ProtocolViolation, testProtocolFixSuggestions)
	}
}

func TestAdvanceStep_TestProtocolViolationRetriesRunTestForMalformedVerdict(t *testing.T) {
	t.Parallel()
	engine, tasks, agents := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	body := initialBody + "\n\n## Test Failures\n\nI recommend changing the route_test gate.\n"
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-proto-malformed",
		Status:    "testing",
		Body:      body,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-malformed", Role: testRunnerRole}},
	})

	err := engine.AdvanceStep("t-proto-malformed", StepOutput{
		StepID:   testVerdictSourceStep,
		Status:   "completed",
		Output:   `{}`,
		AgentID:  "agent-malformed",
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agents.CallCount(); got != 1 {
		t.Fatalf("retry StartAgent calls = %d, want 1", got)
	}
	got, err := tasks.GetTask("t-proto-malformed")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentRuns[0].ProtocolViolation != testProtocolFixSuggestions {
		t.Errorf("agent run protocol violation = %q, want %q", got.AgentRuns[0].ProtocolViolation, testProtocolFixSuggestions)
	}
}

func TestAdvanceStep_TestProtocolViolationAfterRetryStopsWithProtocolReason(t *testing.T) {
	t.Parallel()
	engine, tasks, agents := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	body := initialBody + "\n\n### Failure\nCode evidence: route.go: switch AgentRun.Verdict to VerdictRendered\n"
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
		StepHistory: []StepRecord{{
			StepID: testVerdictSourceStep,
			Status: "failed",
		}},
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-proto-exhausted",
		Status:    "testing",
		Body:      body,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-retry", Role: testRunnerRole}},
	})

	err := engine.AdvanceStep("t-proto-exhausted", StepOutput{
		StepID:   testVerdictSourceStep,
		Status:   "completed",
		Output:   `{"verdict":"FAIL"}`,
		AgentID:  "agent-retry",
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("retry StartAgent calls = %d, want 0 after retry budget exhausted", got)
	}
	got, err := tasks.GetTask("t-proto-exhausted")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t-proto-exhausted"); !strings.Contains(reason, "testing protocol") {
		t.Errorf("status reason = %q, want testing protocol", reason)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", got.Workflow)
	}
	last := got.Workflow.StepHistory[len(got.Workflow.StepHistory)-1]
	if last.StepID != "route_test" || !strings.Contains(last.Output, "protocol violation") {
		t.Errorf("last step = %+v, want route_test protocol violation", last)
	}
	if got.AgentRuns[0].ProtocolViolation != testProtocolFixSuggestions {
		t.Errorf("agent run protocol violation = %q, want %q", got.AgentRuns[0].ProtocolViolation, testProtocolFixSuggestions)
	}
}

func TestRouteTestResult_Pass(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	out, err := runRouteTestResult(e, tasks, "t1", "PASS", time.Now().UTC(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "pass" {
		t.Errorf("output = %q, want pass", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("status = %q, want ready-pr", ti.Status)
	}
}

func TestRouteTestResult_FailUnderCap(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	runs := []AgentRunInfo{{Role: testRunnerRole, StartedAt: now}}
	out, err := runRouteTestResult(e, tasks, "t2", "FAIL", now, runs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "reimplement" {
		t.Errorf("output = %q, want reimplement", out.Output)
	}
	ti, _ := tasks.GetTask("t2")
	if ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", ti.Status)
	}
}

func TestRouteTestResult_FailAtCap(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	runs := []AgentRunInfo{
		{Role: testRunnerRole, StartedAt: now},
		{Role: testRunnerRole, StartedAt: now.Add(time.Minute)},
		{Role: testRunnerRole, StartedAt: now.Add(2 * time.Minute)},
	}
	out, err := runRouteTestResult(e, tasks, "t3", "FAIL", now, runs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "escalated" {
		t.Errorf("output = %q, want escalated", out.Output)
	}
	ti, _ := tasks.GetTask("t3")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestRouteTestResult_ProtocolViolationRunsDoNotCountTowardCap(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	runs := []AgentRunInfo{
		{AgentID: "bad-report", Role: testRunnerRole, StartedAt: now, ProtocolViolation: testProtocolFixSuggestions},
		{AgentID: "valid-1", Role: testRunnerRole, StartedAt: now.Add(time.Minute)},
		{AgentID: "valid-2", Role: testRunnerRole, StartedAt: now.Add(2 * time.Minute)},
	}
	out, err := runRouteTestResult(e, tasks, "t3-protocol", "FAIL", now, runs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "reimplement" {
		t.Errorf("output = %q, want reimplement", out.Output)
	}
	ti, _ := tasks.GetTask("t3-protocol")
	if ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", ti.Status)
	}
}

// TestRouteTestResult_ReDispatch is the regression test for issue #1153: a task
// re-dispatched to testing after a prior cycle already hit the cap must not
// escalate immediately on its first new failure.
func TestRouteTestResult_ReDispatch(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)

	priorCycleEnd := time.Now().UTC().Add(-time.Hour)
	newCycleStart := time.Now().UTC()

	// 3 runs from a prior testing cycle (all before newCycleStart).
	priorRuns := []AgentRunInfo{
		{Role: testRunnerRole, StartedAt: priorCycleEnd.Add(-2 * time.Minute)},
		{Role: testRunnerRole, StartedAt: priorCycleEnd.Add(-time.Minute)},
		{Role: testRunnerRole, StartedAt: priorCycleEnd},
	}
	// 1 new run in the current cycle — must NOT count prior runs toward the cap.
	priorRuns = append(priorRuns, AgentRunInfo{Role: testRunnerRole, StartedAt: newCycleStart.Add(time.Minute)})
	runs := priorRuns

	out, err := runRouteTestResult(e, tasks, "t4", "FAIL", newCycleStart, runs, &newCycleStart)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "reimplement" {
		t.Errorf("output = %q, want reimplement (prior-cycle runs must not count)", out.Output)
	}
	ti, _ := tasks.GetTask("t4")
	if ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", ti.Status)
	}
}

// TestRouteTestResult_ReDispatch_Escalates verifies a re-dispatched task still
// escalates once the new cycle exhausts its own cap.
func TestRouteTestResult_ReDispatch_Escalates(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)

	priorCycleEnd := time.Now().UTC().Add(-time.Hour)
	newCycleStart := time.Now().UTC()

	// 3 prior-cycle runs (must be ignored).
	priorRuns := []AgentRunInfo{
		{Role: testRunnerRole, StartedAt: priorCycleEnd.Add(-2 * time.Minute)},
		{Role: testRunnerRole, StartedAt: priorCycleEnd.Add(-time.Minute)},
		{Role: testRunnerRole, StartedAt: priorCycleEnd},
	}
	// 3 new-cycle runs — cap=3 → escalate.
	newRuns := []AgentRunInfo{
		{Role: testRunnerRole, StartedAt: newCycleStart.Add(time.Minute)},
		{Role: testRunnerRole, StartedAt: newCycleStart.Add(2 * time.Minute)},
		{Role: testRunnerRole, StartedAt: newCycleStart.Add(3 * time.Minute)},
	}
	priorRuns = append(priorRuns, newRuns...)
	runs := priorRuns

	out, err := runRouteTestResult(e, tasks, "t5", "FAIL", newCycleStart, runs, &newCycleStart)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "escalated" {
		t.Errorf("output = %q, want escalated", out.Output)
	}
	ti, _ := tasks.GetTask("t5")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}
