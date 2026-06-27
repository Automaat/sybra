package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
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
	// testVerdictTaintedKey records protocol violations found in a FAIL report.
	// route_test_result uses it to avoid treating a bad test-runner report as
	// evidence that the implementation itself is still broken.
	testVerdictTaintedKey = "tainted"
	// testFailureBodyStartLenKey records the task body length when run_test is
	// dispatched. On completion, Codex structured output only contains
	// {"verdict":"FAIL"}, so the actual failure text must be read from the body
	// delta written by the runner.
	testFailureBodyStartLenKey = "body_start_len"
	testFailuresHeading        = "## Test Failures"

	testProtocolFixSuggestions = "fix-suggestions"
)

// prepareTestVerdictAttemptVars resets per-attempt verdict metadata before a
// test-runner starts. This prevents a protocol violation from a prior retry
// from poisoning a clean retry.
func prepareTestVerdictAttemptVars(wfExec *Execution, stepID, body string) {
	if wfExec == nil || stepID != testVerdictSourceStep {
		return
	}
	wfExec.SetVar("step."+stepID+"."+testFailureBodyStartLenKey, strconv.Itoa(len(body)))
	delete(wfExec.Variables, "step."+stepID+".verdict")
	delete(wfExec.Variables, "step."+stepID+"."+testVerdictTaintedKey)
}

// applyTestVerdictCompletion extracts the machine verdict from run_test output,
// records protocol violations, and turns protocol-violating reports into failed
// steps. The run_agent max_retries budget then retries the tester instead of
// sending implementation agents after a bad report. It returns the violation
// name when one should be persisted on the underlying AgentRun.
func applyTestVerdictCompletion(wfExec *Execution, output *StepOutput, body string) string {
	if wfExec == nil || output == nil || output.StepID != testVerdictSourceStep {
		return ""
	}
	if output.Status != "completed" && output.Status != "failed" {
		return ""
	}
	v := extractTestVerdict(output.Output)
	if (v == "" || v == "FAIL") && containsFixSuggestionsInCurrentTestReport(output.Output, body, wfExec, output.StepID) {
		wfExec.SetVar("step."+output.StepID+"."+testVerdictTaintedKey, testProtocolFixSuggestions)
		output.Status = "failed"
		output.Output = appendTestProtocolViolation(output.Output)
		return testProtocolFixSuggestions
	}
	if output.Status != "completed" {
		return ""
	}
	if v != "" {
		wfExec.SetVar("step."+output.StepID+".verdict", v)
	}
	return ""
}

func appendTestProtocolViolation(output string) string {
	msg := "test-runner protocol violation: FAIL report contained fix suggestions instead of observed symptoms"
	if strings.TrimSpace(output) == "" {
		return msg
	}
	return strings.TrimRight(output, "\n") + "\n\n" + msg
}

// containsFixSuggestionsInCurrentTestReport scans the agent result and the task
// body text added during the current run_test attempt. The body delta matters
// for Codex output_schema runs, where the result is only {"verdict":"FAIL"}.
func containsFixSuggestionsInCurrentTestReport(output, body string, wfExec *Execution, stepID string) bool {
	if containsFixSuggestions(output) {
		return true
	}
	delta, ok := testFailureBodyDelta(body, wfExec, stepID)
	if !ok {
		return false
	}
	if strings.TrimSpace(delta) == "" {
		return false
	}
	if testFailSectionOf(delta) != "" {
		return containsFixSuggestions(delta)
	}
	return failureTextContainsFixSuggestions(delta)
}

func testFailureBodyDelta(body string, wfExec *Execution, stepID string) (string, bool) {
	if body == "" || wfExec == nil {
		return "", false
	}
	raw := wfExec.Variables["step."+stepID+"."+testFailureBodyStartLenKey]
	n, err := strconv.Atoi(raw)
	if raw == "" || err != nil || n < 0 {
		return "", false
	}
	if n >= len(body) {
		return "", true
	}
	return body[n:], true
}

// testFailSectionOf returns the substring starting at "## Test Failures" in
// output, or "" when that heading is absent. Used to narrow full-output scans
// to the part the runner authored under failure context, reducing false
// positives from quoted app error messages or task description prose.
func testFailSectionOf(output string) string {
	idx := strings.Index(strings.ToLower(output), strings.ToLower(testFailuresHeading))
	if idx < 0 {
		return ""
	}
	return output[idx:]
}

// containsFixSuggestions reports whether the test-runner output includes
// imperative fix language in the ## Test Failures section. Such language
// violates the skill contract: test-runners report symptoms and reproduction
// steps; implementers decide the fix.
func containsFixSuggestions(output string) bool {
	section := testFailSectionOf(output)
	if section == "" {
		return false
	}
	return failureTextContainsFixSuggestions(section)
}

func failureTextContainsFixSuggestions(text string) bool {
	for _, line := range fixSuggestionScanLines(text) {
		lower := strings.ToLower(line)
		for _, phrase := range fixSuggestionPhrases {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
		for _, pat := range fixSuggestionPatterns {
			if pat.MatchString(lower) {
				return true
			}
		}
	}
	return false
}

func fixSuggestionScanLines(text string) []string {
	var out []string
	inFence := false
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if skipFixSuggestionLine(trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func skipFixSuggestionLine(trimmed string) bool {
	lower := strings.ToLower(trimmed)
	for _, prefix := range fixSuggestionEvidencePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	if strings.HasPrefix(lower, "code evidence:") && strings.ContainsAny(trimmed, "\"'`") {
		return true
	}
	return false
}

// fixSuggestionPatterns match prescriptive code-edit forms that the test skill
// explicitly prohibits, including the "switch X to Y" form that escaped the
// original task b319d12f implementation.
var fixSuggestionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:change|switch|rename)\s+\S+\s+to\s+\S+`),
	regexp.MustCompile(`\breplace\s+\S+\s+with\s+\S+`),
	regexp.MustCompile(`\buse\s+\S+\s+instead\s+of\s+\S+`),
}

var fixSuggestionPhrases = []string{
	"you should fix",
	"you should change",
	"you should update",
	"you should add",
	"you should remove",
	"you should use",
	"you could fix",
	"you could change",
	"i recommend",
	"i suggest",
	"the fix is",
	"the fix would be",
	"fix by",
	"to fix this",
	"to fix it",
	"consider changing",
	"consider adding",
	"consider using",
	"consider removing",
	"try changing",
	"try adding",
	"try using",
	"should be changed to",
	"needs to be changed",
	"needs to be updated",
	"the implementation should",
	"the code should be",
}

var fixSuggestionEvidencePrefixes = []string{
	"command:",
	"command run:",
	"actual:",
	"actual output:",
	"output:",
	"stdout:",
	"stderr:",
	"expected:",
	"expected output:",
	"verbatim output:",
	"cleanup / control command:",
	"cleanup / control output:",
	"temporary test body",
}

// extractTestVerdict returns "PASS"/"FAIL"/"" from agent output.
//
// Object-shaped output (leading `{` after trimming BOM/whitespace) is treated
// as authoritative JSON: the `verdict` field is parsed and the marker scan is
// skipped entirely. A malformed or unexpected object yields "" → FAIL without
// falling through to the marker, so a JSON body that incidentally contains a
// marker-shaped substring cannot misroute. This is the path codex takes when
// --output-schema enforces a structured response.
//
// Non-object-shaped output (claude plain text) falls to the exact-line marker
// scan. The last matching line wins; missing/ambiguous output yields "" → FAIL,
// which is the safe direction.
func extractTestVerdict(output string) string {
	// Strip a leading UTF-8 BOM, then trim whitespace before shape detection.
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if strings.HasPrefix(s, "{") {
		// Object-shaped: JSON is authoritative. Parse fail → "" (→FAIL).
		// No fall-through to marker scan.
		var v struct {
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			switch strings.ToUpper(strings.TrimSpace(v.Verdict)) {
			case "PASS":
				return "PASS"
			case "FAIL":
				return "FAIL"
			}
		}
		return ""
	}

	// Plain-text path (claude): exact-line marker scan, last match wins.
	verdict := ""
	for line := range strings.SplitSeq(output, "\n") {
		switch strings.TrimSpace(line) {
		case testVerdictPass:
			verdict = "PASS"
		case testVerdictFail:
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

	if violation := wfExec.Variables["step."+testVerdictSourceStep+"."+testVerdictTaintedKey]; violation != "" {
		reason := "test-runner report violated testing protocol after retry: contained fix suggestions instead of observed symptoms"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.protocol-violation", "task_id", taskID, "violation", violation)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "protocol violation: " + violation}, nil
	}

	// Count test-runner runs that belong to the current testing cycle.
	// When a human re-dispatches after human-required, TestingCycleStartedAt is
	// set to the re-dispatch time; runs before that time are from prior cycles
	// and must not inflate the counter. Nil means no re-dispatch has happened
	// (first cycle or automatic implement→test loop), so all runs count.
	attempts := 0
	for i := range t.AgentRuns {
		if t.AgentRuns[i].Role != testRunnerRole {
			continue
		}
		if t.AgentRuns[i].ProtocolViolation != "" {
			continue
		}
		if t.TestingCycleStartedAt != nil && t.AgentRuns[i].StartedAt.Before(*t.TestingCycleStartedAt) {
			continue
		}
		attempts++
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
