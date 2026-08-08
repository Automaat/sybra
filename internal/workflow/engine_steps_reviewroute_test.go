package workflow

import (
	"errors"
	"strconv"
	"strings"
	"testing"
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
