package workflow

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestExtractReviewVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{name: "clean_json", output: `{"verdict":"CLEAN"}`, want: reviewVerdictClean},
		{name: "needs_fixes_lowercase", output: `{"verdict":"needs_fixes"}`, want: reviewVerdictNeedsFixes},
		{name: "json_with_bom", output: "\xef\xbb\xbf{\"verdict\":\"CLEAN\"}", want: reviewVerdictClean},
		{name: "json_with_extra_text", output: "Review Verdict: CLEAN", want: ""},
		{name: "invalid_enum", output: `{"verdict":"APPROVE"}`, want: ""},
		{name: "malformed_json", output: `{"verdict":`, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractReviewVerdict(tc.output); got != tc.want {
				t.Fatalf("ExtractReviewVerdict(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

func TestExecRouteReviewVerdict_MalformedRetriesThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "ready-review",
		Workflow: &Execution{
			WorkflowID:  "simple-task-review",
			CurrentStep: "route_review_verdict",
			State:       ExecRunning,
			Variables: map[string]string{
				reviewVerdictSourceStepVar: "code_review_simple",
			},
		},
	})
	engine := newEngineForEval(t, tasks)
	step := &Step{ID: "route_review_verdict", Type: StepRouteReviewVerdict}

	for attempt := 1; attempt <= reviewVerdictAutoRetryCap; attempt++ {
		ti, _ := tasks.GetTask("t1")
		out, err := engine.execRouteReviewVerdict("t1", step, ti.Workflow, ti)
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d err = %v, want errStepParked", attempt, err)
		}
		if out != (StepOutput{}) {
			t.Fatalf("attempt %d output = %+v, want empty when parked", attempt, out)
		}
		updated, _ := tasks.GetTask("t1")
		if updated.Workflow.CurrentStep != "code_review_simple" {
			t.Fatalf("attempt %d rewound current step = %q, want code_review_simple", attempt, updated.Workflow.CurrentStep)
		}
		if got := updated.Workflow.Variables[reviewReaskNoteVar]; got != reviewMalformedReask {
			t.Fatalf("attempt %d reask note = %q, want %q", attempt, got, reviewMalformedReask)
		}
		if got := updated.Workflow.Variables[reviewVerdictAutoRetryKey("code_review_simple")]; got != strconv.Itoa(attempt) {
			t.Fatalf("attempt %d retry counter = %q, want %q", attempt, got, strconv.Itoa(attempt))
		}
		if got := tasks.Reason("t1"); !strings.Contains(got, "schema-valid verdict") {
			t.Fatalf("attempt %d reason = %q, want schema-valid verdict retry note", attempt, got)
		}
	}

	ti, _ := tasks.GetTask("t1")
	out, err := engine.execRouteReviewVerdict("t1", step, ti.Workflow, ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "malformed verdict — escalated" {
		t.Fatalf("Output = %q, want escalation marker", out.Output)
	}
	updated, _ := tasks.GetTask("t1")
	if updated.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", updated.Status)
	}
	if got := tasks.Reason("t1"); !strings.Contains(got, "schema-valid verdict after auto-retries") {
		t.Fatalf("reason = %q, want schema-valid verdict exhaustion", got)
	}
}

func TestExtractPlanCritiqueVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   string
	}{
		{name: "approve_json", output: `{"verdict":"APPROVE"}`, want: planCritiqueVerdictApprove},
		{name: "refine_lowercase", output: `{"verdict":"refine"}`, want: planCritiqueVerdictRefine},
		{name: "reject_inflected", output: `{"verdict":"REJECTED"}`, want: planCritiqueVerdictReject},
		{name: "plain_markdown_not_json", output: "# Plan Review\n\n## Verdict: REFINE", want: ""},
		{name: "malformed_json", output: `{"verdict":"REJECT"`, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractPlanCritiqueVerdict(tc.output); got != tc.want {
				t.Fatalf("ExtractPlanCritiqueVerdict(%q) = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
}

func TestExecFlagPlanCritique_MalformedRetriesThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "planning",
		PlanCritique: "# Plan Review\n\n## Verdict: APPROVE\n",
		Workflow: &Execution{
			WorkflowID:  "simple-task-plan",
			CurrentStep: "flag_plan_critique_verdict",
			State:       ExecRunning,
			Variables: map[string]string{
				planCritiqueVerdictSourceStepVar: planCritiqueSourceStep,
				planCritiqueVerdictVar:           "",
			},
		},
	})
	engine := newEngineForEval(t, tasks)
	step := &Step{ID: "flag_plan_critique_verdict", Type: StepFlagPlanCritique}

	for attempt := 1; attempt <= planCritiqueVerdictAutoRetryCap; attempt++ {
		ti, _ := tasks.GetTask("t1")
		ti.PlanCritique = ""
		out, err := engine.execFlagPlanCritique("t1", step, ti.Workflow, ti)
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d err = %v, want errStepParked", attempt, err)
		}
		if out != (StepOutput{}) {
			t.Fatalf("attempt %d output = %+v, want empty when parked", attempt, out)
		}
		updated, _ := tasks.GetTask("t1")
		if updated.Workflow.CurrentStep != planCritiqueSourceStep {
			t.Fatalf("attempt %d rewound current step = %q, want %q", attempt, updated.Workflow.CurrentStep, planCritiqueSourceStep)
		}
		if got := updated.Workflow.Variables[planCritiqueReaskNoteVar]; got != planCritiqueMalformedReask {
			t.Fatalf("attempt %d reask note = %q, want %q", attempt, got, planCritiqueMalformedReask)
		}
		if got := tasks.Reason("t1"); !strings.Contains(got, "schema-valid verdict") {
			t.Fatalf("attempt %d reason = %q, want schema-valid verdict retry note", attempt, got)
		}
	}

	ti, _ := tasks.GetTask("t1")
	ti.PlanCritique = ""
	out, err := engine.execFlagPlanCritique("t1", step, ti.Workflow, ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "malformed verdict — escalated" {
		t.Fatalf("Output = %q, want escalation marker", out.Output)
	}
	updated, _ := tasks.GetTask("t1")
	if updated.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", updated.Status)
	}
	if got := tasks.Reason("t1"); !strings.Contains(got, "schema-valid verdict after auto-retries") {
		t.Fatalf("reason = %q, want schema-valid verdict exhaustion", got)
	}
}

func TestPrepareReviewVerdictAttemptVars_DropsThePreviousRoundsVerdict(t *testing.T) {
	t.Parallel()
	wf := &Execution{Variables: map[string]string{
		reviewVerdictVar:           reviewVerdictNeedsFixes,
		reviewVerdictSourceStepVar: "code_review_simple",
		"unrelated":                "keep",
	}}
	prepareReviewVerdictAttemptVars(wf, &Step{ID: "code_review_simple", Config: StepConfig{Role: reviewAgentRole}})

	if got := wf.Variables[reviewVerdictVar]; got != "" {
		t.Fatalf("verdict = %q, want the previous round's answer dropped before a new review runs", got)
	}
	if got := wf.Variables[reviewVerdictSourceStepVar]; got != "code_review_simple" {
		t.Fatalf("source step = %q, want the step being dispatched so a rewind returns to it", got)
	}
	if got := wf.Variables["unrelated"]; got != "keep" {
		t.Fatalf("unrelated var = %q, want it untouched", got)
	}
}

func TestPrepareReviewVerdictAttemptVars_LeavesOtherRolesAlone(t *testing.T) {
	t.Parallel()
	wf := &Execution{Variables: map[string]string{reviewVerdictVar: reviewVerdictClean}}
	prepareReviewVerdictAttemptVars(wf, &Step{ID: "fix_review", Config: StepConfig{Role: "fix-review"}})
	if got := wf.Variables[reviewVerdictVar]; got != reviewVerdictClean {
		t.Fatalf("verdict = %q, want a non-review dispatch to leave it alone", got)
	}
}

func TestAdvanceStep_FailedReviewDoesNotRouteOnTheLastVerdict(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(*mustBuiltinDefinition(t, "simple-task-review")); err != nil {
		t.Fatalf("save simple-task-review: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	wf := &Execution{
		WorkflowID:  "simple-task-review",
		CurrentStep: "code_review_simple",
		State:       ExecRunning,
		Variables: map[string]string{
			reviewVerdictVar:           reviewVerdictNeedsFixes,
			reviewVerdictSourceStepVar: "code_review_simple",
		},
		StartedAt: time.Now().UTC(),
	}
	tasks.Put(TaskInfo{ID: "t-stale", Status: taskstatus.ReadyReview, AgentMode: "headless", Workflow: wf})

	engine.ResumeStalled()

	stored, _ := tasks.GetTask("t-stale")
	if got := stored.Workflow.Variables[reviewVerdictVar]; got != "" {
		t.Fatalf("verdict = %q, want dispatching a fresh review to drop the previous round's answer", got)
	}
}

func TestPrepareReviewVerdictAttemptVars_KeepsTheStaffLaneRewindTarget(t *testing.T) {
	t.Parallel()
	wf := &Execution{Variables: map[string]string{
		reviewVerdictVar:           reviewVerdictNeedsFixes,
		reviewVerdictSourceStepVar: "code_review_staff",
	}}
	prepareReviewVerdictAttemptVars(wf, &Step{ID: "code_review_staff", Config: StepConfig{Role: reviewAgentRole}})

	if got := wf.Variables[reviewVerdictSourceStepVar]; got != "code_review_staff" {
		t.Fatalf("source step = %q, want a staff review to rewind to itself rather than the cheap review", got)
	}
}

func TestRouteReviewVerdict_StopsInsteadOfFixingWhenItEscalated(t *testing.T) {
	t.Parallel()
	def := mustBuiltinDefinition(t, "simple-task-review")
	step := def.StepByID("route_review_verdict")
	if step == nil {
		t.Fatal("route_review_verdict missing from the builtin")
	}
	first := step.Next[0]
	if first.When == nil || first.When.Field != "task.status" ||
		first.When.Value != string(taskstatus.HumanRequired) || first.GoTo != "" {
		t.Fatalf("first edge = %+v, want a human-required stop before the fix edge", first)
	}
}
