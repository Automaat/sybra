package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

// ReviewVerdictFailed is the verdict of a review gate that could not run.
const ReviewVerdictFailed = "FAILED"

const (
	reviewVerdictClean      = "CLEAN"
	reviewVerdictNeedsFixes = "NEEDS_FIXES"

	// reviewVerdictVar / reviewVerdictSourceStepVar are set in engine_advance
	// when a review-role run_agent step completes: the extracted structured
	// verdict (see ExtractReviewVerdict, possibly "" for malformed/missing),
	// and the step ID that produced it (code_review_simple or
	// review_simple) so route_review_verdict can rewind to the right
	// step on retry.
	reviewVerdictVar           = "review_verdict"
	reviewAgentRole            = "review"
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

	// reviewGateFailedReask tells a retry why the previous round produced no
	// usable verdict, which is not a formatting problem.
	reviewGateFailedReask = "Your previous review returned FAILED: the gate could not run. Fetch the base ref if it " +
		"did not resolve, then run the review again and return a verdict."
)

// ExtractReviewVerdict returns "CLEAN"/"NEEDS_FIXES"/"FAILED"/"" from a review-role
// agent's final message.
//
// The message is expected to be the output_schema-enforced JSON object
// `{"verdict":"CLEAN"|"NEEDS_FIXES"|"FAILED"}` (see simple-task-review.yaml's
// code_review_simple step). A non-object message, or an
// object whose verdict field doesn't match one of the known values,
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
	case ReviewVerdictFailed:
		return ReviewVerdictFailed
	default:
		return ""
	}
}

func reviewVerdictAutoRetryKey(sourceStep string) string {
	return "step." + sourceStep + ".review_verdict_retry"
}

// prepareReviewVerdictAttemptVars drops the previous round's verdict before a
// review agent is dispatched. The verdict is only written back when that agent
// completes, so without this a review that dies before producing one leaves
// the last round's answer in place and route_review_verdict acts on it as if
// it were fresh. Mirrors prepareTestVerdictAttemptVars on the testing route.
//
// The source step is re-pointed rather than dropped: both the verdict router
// and require_code_review rewind to whatever it names.
func prepareReviewVerdictAttemptVars(wfExec *Execution, step *Step) {
	if wfExec == nil || step == nil || step.Config.Role != reviewAgentRole {
		return
	}
	delete(wfExec.Variables, reviewVerdictVar)
	wfExec.SetVar(reviewVerdictSourceStepVar, step.ID)
}

// execRouteReviewVerdict is a mechanical (no-LLM) router that reads the
// review-role step's structured verdict (stashed by engine_advance into
// reviewVerdictVar) and persists it onto the task so a later re-triggered
// workflow can see it without re-parsing the review sidecar markdown.
//
// A valid CLEAN/NEEDS_FIXES verdict is a pass-through: the step's own
// `next.when` clauses in simple-task-review.yaml route on vars.review_verdict.
// A FAILED verdict means the review gate itself could not run, and like a
// missing/malformed verdict it never falls through to either branch — it
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

	// An earlier round's verdict describes a revision this round failed to
	// review, so it must not survive to skip the next attempt.
	if err := e.tasks.SetCodeReviewVerdict(taskID, ""); err != nil {
		return StepOutput{}, err
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
			if verdict == ReviewVerdictFailed {
				wfExec.SetVar(reviewReaskNoteVar, reviewGateFailedReask)
			} else {
				wfExec.SetVar(reviewReaskNoteVar, reviewMalformedReask)
			}
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
	if verdict == ReviewVerdictFailed {
		reason = "code review returned FAILED after auto-retries: the review gate could not run — needs human inspection of the review sidecar"
	}
	if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Warn("workflow.review.malformed-verdict.escalate", "task_id", taskID, "step", sourceStep)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "malformed verdict — escalated"}, nil
}
