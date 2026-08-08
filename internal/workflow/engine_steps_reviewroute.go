package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	reviewVerdictClean      = "CLEAN"
	reviewVerdictNeedsFixes = "NEEDS_FIXES"

	// reviewVerdictVar / reviewVerdictSourceStepVar are set in engine_advance
	// when a review-role run_agent step completes: the extracted structured
	// verdict (see ExtractReviewVerdict, possibly "" for malformed/missing),
	// and the step ID that produced it (code_review_simple or
	// code_review_staff) so route_review_verdict can rewind to the right
	// step on retry.
	reviewVerdictVar           = "review_verdict"
	reviewVerdictSourceStepVar = "review_verdict_source_step"

	// reviewVerdictAutoRetryCap bounds how many times route_review_verdict
	// re-runs the review agent on a malformed/missing structured verdict
	// before escalating to human-required. Mirrors testingAutoRetryCap's
	// posture: a schema-conformance failure describes the agent's output
	// discipline, not the code under review, so it gets a small bounded
	// auto-retry rather than immediately burning a human's attention.
	reviewVerdictAutoRetryCap     = 2
	reviewVerdictAutoRetryBackoff = 2 * time.Minute

	// reviewReaskNoteVar carries the schema-conformance correction into the
	// re-run review prompt (see simple-task-review.yaml).
	reviewReaskNoteVar = "review_reask_note"

	reviewMalformedReask = "Your previous final message could not be parsed as a valid verdict. " +
		"Write the full review to the sidecar file as instructed, but your FINAL message of the " +
		"turn must be ONLY this JSON object (no prose, no markdown fences, no extra keys): " +
		`{"verdict":"CLEAN"} or {"verdict":"NEEDS_FIXES"}.`
)

// ExtractReviewVerdict returns "CLEAN"/"NEEDS_FIXES"/"" from a review-role
// agent's final message.
//
// The message is expected to be the output_schema-enforced JSON object
// `{"verdict":"CLEAN"|"NEEDS_FIXES"}` (see simple-task-review.yaml's
// code_review_simple/code_review_staff steps). A non-object message, or an
// object whose verdict field doesn't match one of the two known values,
// returns "" — callers must treat that as "no verdict", never silently
// assume NEEDS_FIXES. This intentionally does not fall back to scanning for
// a legacy "Review Verdict: ..." text marker: that literal-prefix parsing is
// exactly the format-drift failure mode this replaces (see #2761), so a
// provider that doesn't return schema-valid JSON goes through the bounded
// retry/escalate path in execRouteReviewVerdict instead.
func ExtractReviewVerdict(output string) string {
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var v struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(v.Verdict)) {
	case reviewVerdictClean:
		return reviewVerdictClean
	case reviewVerdictNeedsFixes:
		return reviewVerdictNeedsFixes
	default:
		return ""
	}
}

func reviewVerdictAutoRetryKey(sourceStep string) string {
	return "step." + sourceStep + ".review_verdict_retry"
}

// execRouteReviewVerdict is a mechanical (no-LLM) router that reads the
// review-role step's structured verdict (stashed by engine_advance into
// reviewVerdictVar) and persists it onto the task so a later re-triggered
// workflow can see it without re-parsing the review sidecar markdown.
//
// A valid CLEAN/NEEDS_FIXES verdict is a pass-through: the step's own
// `next.when` clauses in simple-task-review.yaml route on vars.review_verdict.
// A missing/malformed verdict never falls through to either branch — it
// bounded-retries the review agent (feeding back a schema-conformance reask
// note) via rewindRetry, then escalates to human-required once the retry
// budget is spent.
func (e *Engine) execRouteReviewVerdict(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	verdict := wfExec.Variables[reviewVerdictVar]
	switch verdict {
	case reviewVerdictClean, reviewVerdictNeedsFixes:
		delete(wfExec.Variables, reviewReaskNoteVar)
		if err := e.tasks.SetCodeReviewVerdict(taskID, verdict); err != nil {
			return StepOutput{}, err
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: verdict}, nil
	}

	sourceStep := wfExec.Variables[reviewVerdictSourceStepVar]
	if sourceStep == "" {
		sourceStep = "code_review_simple"
	}
	armed, attempt, err := e.rewindRetry(taskID, wfExec, t, rewindRetryPolicy{
		counterKey: reviewVerdictAutoRetryKey(sourceStep),
		max:        reviewVerdictAutoRetryCap,
		rewindStep: sourceStep,
		backoff:    func(int) time.Duration { return reviewVerdictAutoRetryBackoff },
		onArm: func(wfExec *Execution, _ int) {
			wfExec.SetVar(reviewReaskNoteVar, reviewMalformedReask)
			delete(wfExec.Variables, reviewVerdictVar)
		},
		reason: func(attempt int) string {
			return fmt.Sprintf("re-running code review (attempt %d/%d): previous response did not return a schema-valid verdict",
				attempt, reviewVerdictAutoRetryCap)
		},
	})
	if err != nil {
		return StepOutput{}, err
	}
	if armed {
		e.logger.Warn("workflow.review.malformed-verdict.retry", "task_id", taskID, "step", sourceStep, "attempt", attempt)
		return StepOutput{}, errStepParked
	}

	reason := "code review did not return a schema-valid verdict after auto-retries — needs human inspection of the review sidecar"
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Warn("workflow.review.malformed-verdict.escalate", "task_id", taskID, "step", sourceStep)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "malformed verdict — escalated"}, nil
}
