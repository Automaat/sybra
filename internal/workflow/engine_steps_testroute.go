package workflow

import (
	"fmt"
	"strings"
)

const (
	// testVerdictSourceStep is the run_agent step whose verdict route_test_result
	// inspects. The builtin testing-task workflow names its test-runner step
	// exactly this.
	testVerdictSourceStep = "run_test"
	// testVerdictPass / testVerdictFail are the markers the test-runner prints on
	// its final line. The verdict is extracted from the UNtruncated agent output
	// into a dedicated `step.<id>.verdict` var (see engine_advance), so a long
	// PASS summary is never lost to output truncation.
	testVerdictPass = "TEST_VERDICT: PASS"
	testVerdictFail = "TEST_VERDICT: FAIL"
	// testRunnerRole counts prior attempts; mirrors agent.RoleTestRunner
	// (literal to avoid importing the agent package into the engine).
	testRunnerRole = "test-runner"
)

// extractTestVerdict returns "PASS"/"FAIL"/"" by scanning output for the verdict
// marker. The last matching line wins (the skill prints it last), and the scan
// runs over the untruncated agent result so the marker survives long summaries.
func extractTestVerdict(output string) string {
	verdict := ""
	for line := range strings.SplitSeq(output, "\n") {
		switch {
		case strings.Contains(line, testVerdictPass):
			verdict = "PASS"
		case strings.Contains(line, testVerdictFail):
			verdict = "FAIL"
		}
	}
	return verdict
}

// execRouteTestResult routes a task after its adversarial test-runner finishes.
//
// The test-runner's job is to PROVE the implementation does not satisfy the
// task. It prints testVerdictPass only when it failed to break the feature.
//
//   - pass  → ready-pr   (a separate workflow opens the PR)
//   - fail  → in-progress (re-implement, carrying the agent's test-failure
//     notes in the task body) — until the attempt cap, then human-required.
//
// Counting prior test-runner runs (which persist on the task across the
// implement→review→test loop) gives a natural, stateless attempt counter:
// the just-finished run is already recorded, so the Nth failure sees N runs.
func (e *Engine) execRouteTestResult(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Read the untruncated verdict var (set in engine_advance from the full
	// agent output). Anything other than PASS — including a missing verdict from
	// a crashed/empty run — is treated as a failure, conservative by design.
	if wfExec.Variables["step."+testVerdictSourceStep+".verdict"] == "PASS" {
		if err := e.tasks.UpdateTaskStatus(taskID, "ready-pr", "manual testing passed"); err != nil {
			return StepOutput{}, err
		}
		e.logger.Info("workflow.test.passed", "task_id", taskID, "step", step.ID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "pass"}, nil
	}

	attempts := 0
	for i := range t.AgentRuns {
		if t.AgentRuns[i].Role == testRunnerRole {
			attempts++
		}
	}
	limit := e.maxTestAttempts
	if limit <= 0 {
		limit = defaultTestAttempts
	}

	if attempts >= limit {
		reason := fmt.Sprintf("manual testing failed %d×: feature still does not match the task — needs a human", attempts)
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.escalate", "task_id", taskID, "attempts", attempts, "cap", limit)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "escalated"}, nil
	}

	// The branch was already code-reviewed before it reached testing. A
	// test-failure re-implementation must NOT re-run agentic code review — mark
	// the task reviewed so simple-task-review's maybe_review short-circuits
	// straight to testing on the next pass. Idempotent if already set.
	if err := e.tasks.MarkTaskReviewed(taskID); err != nil {
		e.logger.Warn("workflow.test.mark-reviewed", "task_id", taskID, "err", err)
	}
	reason := "manual testing found defects — re-implementing (see ## Test Failures in the task)"
	if err := e.tasks.UpdateTaskStatus(taskID, "in-progress", reason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Info("workflow.test.reimplement", "task_id", taskID, "attempts", attempts, "cap", limit)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "reimplement"}, nil
}
