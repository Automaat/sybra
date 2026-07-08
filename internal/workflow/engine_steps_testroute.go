package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	// older structured-output runs returned only {"verdict":"FAIL"}, so the
	// actual failure text had to be read from the body delta written by the
	// runner.
	testFailureBodyStartLenKey = "body_start_len"
	testVerdictOutcomeKey      = "outcome"
	testFailureFingerprintKey  = "failure_fingerprint"
	testFailuresHeading        = "## Test Failures"
	// resolvedTestFailuresHeading is what a stale "## Test Failures" section
	// is renamed to when a newer cycle supersedes it (see
	// stripTestFailuresSections). Deliberately distinct from testFailuresHeading
	// so agents pattern-matching for the live heading never mistake archived,
	// already-superseded reports for the current blocking failure.
	resolvedTestFailuresHeading = "## Resolved Test Failures (historical)"

	testOutcomePass                 = "pass"
	testOutcomeProductBug           = "product_bug"
	testOutcomeAmbiguousRequirement = "ambiguous_requirement"
	testOutcomeInfraFailure         = "infra_failure"
	testOutcomeMissingEvidence      = "missing_evidence"
	testOutcomeProtocolViolation    = "protocol_violation"

	testProtocolFixSuggestions  = "fix-suggestions"
	testProtocolMissingEvidence = "missing-evidence"
)

const (
	// testingAutoRetryCap bounds how many times route_test re-runs the tester
	// for transient (infra) or agent-behaviour (missing-evidence) outcomes
	// before handing the task to a human. These outcomes describe the *runner*,
	// not the implementation, so a bounded auto-retry recovers most of them
	// without a human — a batch of tasks fanning into testing at once no longer
	// piles up in human-required on one-off runner flakiness.
	testingAutoRetryCap = 2
	// testingAutoRetryBackoff spaces re-dispatches so a persistently broken
	// sandbox or provider is not hammered in a tight loop; ResumeStalled honours
	// it via workflowRetryAfterVar.
	testingAutoRetryBackoff = 3 * time.Minute
	// testingReaskNoteVar carries a targeted instruction into the re-run tester
	// prompt (see testing-task.yaml). Set on a missing-evidence retry so the
	// runner is told exactly which machine-checkable evidence its prior report
	// lacked, rather than being re-run blind.
	testingReaskNoteVar = "testing_reask_note"

	missingEvidenceReask = "Your previous FAIL report was rejected because it lacked machine-checkable " +
		"evidence. For EVERY claimed defect you MUST include: the exact command you ran, its verbatim " +
		"output, the expected behaviour citing the task's own words, and a code line quoted from the " +
		"CURRENT file (file:line). Re-run the probes and produce that evidence — or, if the feature " +
		"actually works, emit PASS with the required manual-testing evidence."
)

func testingAutoRetryKey(outcome string) string {
	return "step." + testVerdictSourceStep + ".auto_retry." + outcome
}

// parkTestingRetryOrEscalate re-arms the run_test step for another adversarial
// run when a transient/agent-behaviour outcome still has retry budget, spacing
// the re-dispatch with a backoff that ResumeStalled honours. It returns
// parked=true after scheduling a retry — the caller MUST return errStepParked so
// executeSteps parks without advancing — and parked=false when the cap is
// exhausted and the caller should escalate to human-required. The retry counter
// is keyed by outcome and persists on the workflow across the rewind.
func (e *Engine) parkTestingRetryOrEscalate(taskID, outcome, reaskNote string, wfExec *Execution, t TaskInfo) (parked bool, err error) {
	key := testingAutoRetryKey(outcome)
	attempts := parseWorkflowInt(wfExec.Variables[key])
	if attempts >= testingAutoRetryCap {
		return false, nil
	}
	wfExec.SetVar(key, strconv.Itoa(attempts+1))
	wfExec.SetVar(workflowRetryAfterVar, time.Now().UTC().Add(testingAutoRetryBackoff).Format(time.RFC3339))
	if reaskNote != "" {
		wfExec.SetVar(testingReaskNoteVar, reaskNote)
	}
	// Clear the prior run's verdict/outcome/taint so the re-armed run is judged
	// on its own output, not the stale report that triggered this retry.
	clearTestVerdictVars(wfExec)
	// Also clear run_test's step-history records: CountStep(run_test) counts
	// every historical execution, not just the current retry cycle, so without
	// this a route-level re-arm would leave the tester's own in-step
	// max_retries budget looking exhausted from earlier cycles.
	wfExec.ClearStepRecords(testVerdictSourceStep)
	// Rewind to the tester step; ResumeStalled re-dispatches it once idle and
	// past the backoff (run_test is a run_agent step).
	wfExec.CurrentStep = testVerdictSourceStep
	wfExec.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return false, err
	}
	reason := fmt.Sprintf("auto-retrying adversarial testing (%s, attempt %d/%d)", outcome, attempts+1, testingAutoRetryCap)
	if err := e.tasks.UpdateTaskStatus(taskID, t.Status, reason); err != nil {
		return false, err
	}
	e.logger.Info("workflow.test.auto-retry",
		"task_id", taskID, "outcome", outcome, "attempt", attempts+1, "cap", testingAutoRetryCap)
	return true, nil
}

// retryOrEscalateTransient auto-retries a transient/agent-behaviour test outcome
// while retry budget remains (returning errStepParked so executeSteps parks),
// otherwise escalates the task to human-required with humanReason and returns a
// completed step output carrying doneOutput. logMsg names the escalation event.
func (e *Engine) retryOrEscalateTransient(taskID, stepID, outcome, reask, humanReason, doneOutput, logMsg string, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	parked, err := e.parkTestingRetryOrEscalate(taskID, outcome, reask, wfExec, t)
	if err != nil {
		return StepOutput{}, err
	}
	if parked {
		return StepOutput{}, errStepParked
	}
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", humanReason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Warn(logMsg, "task_id", taskID)
	return StepOutput{StepID: stepID, Status: "completed", Output: doneOutput}, nil
}

func clearTestVerdictVars(wfExec *Execution) {
	if wfExec == nil || wfExec.Variables == nil {
		return
	}
	for _, suffix := range []string{".verdict", "." + testVerdictOutcomeKey, "." + testVerdictTaintedKey, "." + testFailureFingerprintKey} {
		delete(wfExec.Variables, "step."+testVerdictSourceStep+suffix)
	}
}

type structuredTestOutput struct {
	Verdict           string                     `json:"verdict"`
	Outcome           string                     `json:"outcome,omitempty"`
	FailuresMarkdown  string                     `json:"failures_markdown,omitempty"`
	SurfaceKind       string                     `json:"surface_kind,omitempty"`
	AppStarted        flexBool                   `json:"app_started,omitempty"`
	StartCommand      string                     `json:"start_command,omitempty"`
	ReadinessProbe    readinessProbeEvidence     `json:"readiness_probe,omitzero"`
	ManualProbes      manualProbeEvidenceList    `json:"manual_probes,omitempty"`
	AutomatedChecks   automatedCheckEvidenceList `json:"automated_checks,omitempty"`
	UnableToRunReason string                     `json:"unable_to_run_reason,omitempty"`
}

type readinessProbeEvidence struct {
	Command  string       `json:"command"`
	Actual   evidenceText `json:"actual"`
	Output   evidenceText `json:"output"`
	Observed evidenceText `json:"observed"`
	Status   evidenceText `json:"status"`
	Raw      string       `json:"-"`
}

type manualProbeEvidenceList []manualProbeEvidence

type manualProbeEvidence struct {
	Command  string       `json:"command"`
	Expected evidenceText `json:"expected"`
	Actual   evidenceText `json:"actual"`
	Output   evidenceText `json:"output"`
	Observed evidenceText `json:"observed"`
	Status   evidenceText `json:"status"`
	Raw      string       `json:"-"`
}

type automatedCheckEvidenceList []automatedCheckEvidence

type automatedCheckEvidence struct {
	Command  string       `json:"command"`
	Actual   evidenceText `json:"actual"`
	Output   evidenceText `json:"output"`
	Observed evidenceText `json:"observed"`
	Status   evidenceText `json:"status"`
	Raw      string       `json:"-"`
}

type evidenceText string

// flexBool tolerates LLM JSON drift on boolean verdict fields: a model often
// emits app_started as the quoted string "true"/"false" instead of a JSON
// bool. A strict bool unmarshal would fail the WHOLE structuredTestOutput
// object and discard an otherwise-valid PASS verdict — misclassifying a passing
// test as infra_failure. Accept either shape.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var bv bool
	if err := json.Unmarshal(data, &bv); err == nil {
		*b = flexBool(bv)
		return nil
	}
	var sv string
	if err := json.Unmarshal(data, &sv); err != nil {
		return err
	}
	// Models often decorate the boolean with a parenthetical justification, e.g.
	// `"true (component.Start() run as a goroutine)"`. Judge the leading token so
	// an affirmative answer is not silently read as false.
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(sv)))
	if len(fields) == 0 {
		*b = false
		return nil
	}
	switch strings.Trim(fields[0], "(),.:;\"'`") {
	case "true", "yes", "y", "t", "1":
		*b = true
	default:
		*b = false
	}
	return nil
}

func (l *manualProbeEvidenceList) UnmarshalJSON(data []byte) error {
	*l = nil
	if strings.TrimSpace(string(data)) == "null" {
		return nil
	}
	raw, ok, err := unmarshalEvidenceString(data)
	if err != nil || ok {
		if raw != "" {
			*l = manualProbeEvidenceList{{Raw: raw}}
		}
		return err
	}
	var one manualProbeEvidence
	if err := json.Unmarshal(data, &one); err == nil {
		*l = manualProbeEvidenceList{one}
		return nil
	} else if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return err
	}
	var many []manualProbeEvidence
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

func (e *manualProbeEvidence) UnmarshalJSON(data []byte) error {
	if raw, ok, err := unmarshalEvidenceString(data); err != nil || ok {
		e.Raw = raw
		return err
	}
	type alias manualProbeEvidence
	if err := json.Unmarshal(data, (*alias)(e)); err != nil {
		return err
	}
	e.fillAliases()
	return nil
}

func (l *automatedCheckEvidenceList) UnmarshalJSON(data []byte) error {
	*l = nil
	if strings.TrimSpace(string(data)) == "null" {
		return nil
	}
	raw, ok, err := unmarshalEvidenceString(data)
	if err != nil || ok {
		if raw != "" {
			*l = automatedCheckEvidenceList{{Raw: raw}}
		}
		return err
	}
	var one automatedCheckEvidence
	if err := json.Unmarshal(data, &one); err == nil {
		*l = automatedCheckEvidenceList{one}
		return nil
	} else if strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return err
	}
	var many []automatedCheckEvidence
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*l = many
	return nil
}

func (e *automatedCheckEvidence) UnmarshalJSON(data []byte) error {
	if raw, ok, err := unmarshalEvidenceString(data); err != nil || ok {
		e.Raw = raw
		return err
	}
	type alias automatedCheckEvidence
	if err := json.Unmarshal(data, (*alias)(e)); err != nil {
		return err
	}
	e.fillAliases()
	return nil
}

func (e *readinessProbeEvidence) UnmarshalJSON(data []byte) error {
	if raw, ok, err := unmarshalEvidenceString(data); err != nil || ok {
		e.Raw = raw
		return err
	}
	type alias readinessProbeEvidence
	if err := json.Unmarshal(data, (*alias)(e)); err != nil {
		return err
	}
	e.fillAliases()
	return nil
}

func (e *readinessProbeEvidence) fillAliases() {
	if strings.TrimSpace(string(e.Actual)) == "" {
		e.Actual = firstNonEmptyText(e.Observed, e.Output, e.Status)
	}
}

func (e *readinessProbeEvidence) text() string {
	return strings.Join(collectNonEmptyStrings(
		e.Command,
		string(e.Actual),
		string(e.Output),
		string(e.Observed),
		string(e.Status),
		e.Raw,
	), "\n")
}

func (e *manualProbeEvidence) fillAliases() {
	if strings.TrimSpace(string(e.Actual)) == "" {
		e.Actual = firstNonEmptyText(e.Observed, e.Output, e.Status)
	}
}

func (e *automatedCheckEvidence) fillAliases() {
	if strings.TrimSpace(string(e.Actual)) == "" {
		e.Actual = firstNonEmptyText(e.Observed, e.Output, e.Status)
	}
}

func (e *evidenceText) UnmarshalJSON(data []byte) error {
	if raw, ok, err := unmarshalEvidenceString(data); err != nil || ok {
		*e = evidenceText(raw)
		return err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = evidenceText(strings.Join(collectEvidenceStrings(value), "\n"))
	return nil
}

func unmarshalEvidenceString(data []byte) (raw string, ok bool, err error) {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		return s, true, nil
	} else if strings.HasPrefix(strings.TrimSpace(string(data)), `"`) {
		return "", true, err
	}
	return "", false, nil
}

func collectEvidenceStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"command", "expected", "actual", "observed", "output", "status", "url"} {
			if value, ok := x[key]; ok {
				out = append(out, collectEvidenceStrings(value)...)
			}
		}
	case []any:
		for _, item := range x {
			out = append(out, collectEvidenceStrings(item)...)
		}
	case string:
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimSpace(x))
		}
	case float64, bool:
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func firstNonEmptyText(values ...evidenceText) evidenceText {
	for _, value := range values {
		if strings.TrimSpace(string(value)) != "" {
			return value
		}
	}
	return ""
}

func collectNonEmptyStrings(values ...string) []string {
	var out []string
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

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
	delete(wfExec.Variables, "step."+stepID+"."+testVerdictOutcomeKey)
	delete(wfExec.Variables, "step."+stepID+"."+testFailureFingerprintKey)
}

func (e *Engine) prepareTestStepCompletion(taskID string, t TaskInfo, output *StepOutput, wfExec *Execution, body *string) error {
	if appended, nextBody, appendErr := e.appendTestFailureReport(taskID, *output, wfExec, *body); appendErr != nil {
		return appendErr
	} else if appended {
		*body = nextBody
	}

	violation, outcome, fingerprint := applyTestVerdictCompletion(wfExec, output, *body, t)
	if output.AgentID != "" && outcome != "" {
		if err := e.tasks.MarkAgentRunTestOutcome(taskID, output.AgentID, outcome, fingerprint); err != nil {
			return fmt.Errorf("mark test outcome: %w", err)
		}
	}
	if output.AgentID != "" && violation != "" {
		if err := e.tasks.MarkAgentRunProtocolViolation(taskID, output.AgentID, violation); err != nil {
			return fmt.Errorf("mark test protocol violation: %w", err)
		}
	}
	return nil
}

func (e *Engine) appendTestFailureReport(taskID string, output StepOutput, wfExec *Execution, body string) (appended bool, nextBody string, err error) {
	if wfExec == nil || output.StepID != testVerdictSourceStep {
		return false, body, nil
	}
	if output.Status != "completed" {
		return false, body, nil
	}

	report := ""
	isFail := false
	if parsed, ok := parseStructuredTestOutput(output.Output); ok {
		if strings.ToUpper(strings.TrimSpace(parsed.Verdict)) != "FAIL" {
			return false, body, nil
		}
		isFail = true
		report = normalizeStructuredFailuresMarkdown(parsed.FailuresMarkdown, parsed.Outcome)
	} else if ExtractTestVerdict(output.Output) == "FAIL" {
		isFail = true
		report = plainTestFailureReport(output.Output)
	}
	if !isFail {
		return false, body, nil
	}
	if delta, hasDelta := testFailureBodyDelta(body, wfExec, output.StepID); hasDelta && testFailSectionOf(delta) != "" {
		var currentStart int
		nextBody, currentStart = normalizeTestFailureDeltaBody(body, delta)
		wfExec.SetVar("step."+output.StepID+"."+testFailureBodyStartLenKey, strconv.Itoa(currentStart))
		if err := e.tasks.ReplaceTaskBody(taskID, nextBody); err != nil {
			return false, body, fmt.Errorf("normalize test failure report: %w", err)
		}
		return true, nextBody, nil
	}
	if report == "" {
		return false, body, nil
	}

	// Strip any prior "## Test Failures" section(s) before appending the new
	// one, so at most one is ever live in the body — it is then unambiguously
	// the current, blocking failure. Priors are archived under a distinctly
	// different heading rather than dropped, preserving audit history without
	// reintroducing the ambiguity.
	strippedBody, priorSections := stripTestFailuresSections(body)
	nextBody = strippedBody
	for _, prior := range priorSections {
		nextBody = appendRawBody(nextBody, archiveTestFailuresSection(prior))
	}
	nextBody = appendRawBody(nextBody, report)
	if err := e.tasks.ReplaceTaskBody(taskID, nextBody); err != nil {
		return false, body, fmt.Errorf("append test failure report: %w", err)
	}
	return true, nextBody, nil
}

func normalizeTestFailureDeltaBody(body, delta string) (nextBody string, currentStart int) {
	preAttemptBody := body[:len(body)-len(delta)]
	strippedPreAttemptBody, priorSections := stripTestFailuresSections(preAttemptBody)
	strippedDelta, deltaSections := stripTestFailuresSections(delta)
	if len(deltaSections) == 0 {
		return body, len(body)
	}

	nextBody = strippedPreAttemptBody
	nextBody = appendRawBody(nextBody, strippedDelta)
	for _, prior := range priorSections {
		nextBody = appendRawBody(nextBody, archiveTestFailuresSection(prior))
	}
	for _, prior := range deltaSections[:len(deltaSections)-1] {
		nextBody = appendRawBody(nextBody, archiveTestFailuresSection(prior))
	}
	currentSection := deltaSections[len(deltaSections)-1]
	nextBody = appendRawBody(nextBody, currentSection)
	currentStart = strings.LastIndex(nextBody, currentSection)
	if currentStart < 0 {
		currentStart = len(nextBody)
	}
	return nextBody, currentStart
}

// stripTestFailuresSections removes every "## Test Failures" section from
// body (heading line through the line before the next top-level "## "
// heading, or end of body) and returns the remaining body plus the removed
// section contents in document order. Headings inside fenced code blocks are
// ignored, matching testFailSectionOf's fence handling.
func stripTestFailuresSections(body string) (remaining string, removed []string) {
	lines := strings.Split(body, "\n")
	var out []string
	inFence := false
	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			out = append(out, lines[i])
			i++
			continue
		}
		if !inFence && isTestFailuresHeading(trimmed) {
			start := i
			j := i + 1
			sectionInFence := false
			for j < len(lines) {
				jTrimmed := strings.TrimSpace(lines[j])
				if strings.HasPrefix(jTrimmed, "```") || strings.HasPrefix(jTrimmed, "~~~") {
					sectionInFence = !sectionInFence
					j++
					continue
				}
				if !sectionInFence && strings.HasPrefix(jTrimmed, "## ") {
					break
				}
				j++
			}
			removed = append(removed, strings.TrimSpace(strings.Join(lines[start:j], "\n")))
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	remaining = strings.TrimSpace(strings.Join(out, "\n"))
	return remaining, removed
}

// archiveTestFailuresSection renames a removed section's heading line from
// "## Test Failures" to resolvedTestFailuresHeading, preserving the rest of
// its content for audit.
func archiveTestFailuresSection(section string) string {
	lines := strings.SplitN(section, "\n", 2)
	if len(lines) == 1 {
		return resolvedTestFailuresHeading
	}
	return resolvedTestFailuresHeading + "\n" + lines[1]
}

func plainTestFailureReport(output string) string {
	section := testFailSectionOf(output)
	if section == "" {
		return ""
	}
	return stripTestVerdictMarkers(section)
}

func parseStructuredTestOutput(output string) (structuredTestOutput, bool) {
	for _, candidate := range structuredTestOutputCandidates(output) {
		var parsed structuredTestOutput
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
			// The corruption (e.g. an unescaped quote inside failures_markdown)
			// is almost never in the verdict field itself, so regex-extract it
			// directly rather than discarding a confirmed verdict. Recover
			// outcome/failures_markdown the same way — without them a
			// confirmed FAIL still loses its report and gets misclassified as
			// missing_evidence instead of the diagnosed product_bug/etc.
			if v := extractVerdictFieldRegex(candidate); v != "" {
				return structuredTestOutput{
					Verdict:          v,
					Outcome:          extractOutcomeFieldRegex(candidate),
					FailuresMarkdown: extractFailuresMarkdownFieldRegex(candidate),
				}, true
			}
			continue
		}
		if strings.TrimSpace(parsed.Verdict) == "" {
			continue
		}
		return parsed, true
	}
	return structuredTestOutput{}, false
}

func structuredTestOutputCandidates(output string) []string {
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if strings.HasPrefix(s, "{") {
		return []string{s}
	}
	var candidates []string
	for _, candidate := range fencedCodeBlockCandidates(s) {
		// The structured test report is always a JSON object. Ignore fenced
		// snippets/logs that merely mention `"verdict":"PASS|FAIL"` so the
		// regex fallback cannot misclassify unrelated blocks as the final report.
		if strings.HasPrefix(strings.TrimSpace(candidate), "{") {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// testVerdictFencedBlockRe extracts the contents of a fenced code block
// (```json ... ``` or ``` ... ```). Unlike a line-by-line scan, this matches
// even when the closing ``` immediately follows content with no newline
// before it (e.g. prose-wrapped output like "...text ```json\n{...}```").
var testVerdictFencedBlockRe = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*(.*?)```")

func fencedCodeBlockCandidates(output string) []string {
	var candidates []string
	for _, m := range testVerdictFencedBlockRe.FindAllStringSubmatch(output, -1) {
		if candidate := strings.TrimSpace(m[1]); candidate != "" {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

// testVerdictFieldRe pulls "verdict": "PASS"/"FAIL" directly out of raw text,
// used as a fallback when a full JSON unmarshal fails.
var testVerdictFieldRe = regexp.MustCompile(`(?i)"verdict"\s*:\s*"(pass|fail)"`)

func extractVerdictFieldRegex(s string) string {
	m := testVerdictFieldRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// testOutcomeFieldRe pulls "outcome": "..." out of raw text, used alongside
// testVerdictFieldRe when a full JSON unmarshal fails.
var testOutcomeFieldRe = regexp.MustCompile(`(?i)"outcome"\s*:\s*"([^"]*)"`)

func extractOutcomeFieldRegex(s string) string {
	m := testOutcomeFieldRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// testFailuresMarkdownFieldRe recovers the failures_markdown field's value
// when json.Unmarshal fails because of an unescaped quote inside it — the
// corruption this fallback exists for. Since the value's own quotes can no
// longer be trusted to delimit it, this captures non-greedily up to whichever
// comes first: the object's closing brace, or the start of the next known
// structuredTestOutput field (per the test-runner's documented output
// contract, failures_markdown is rarely the last field written — surface_kind,
// app_started, manual_probes, etc. commonly follow it). Stopping at the first
// recognized boundary avoids absorbing those trailing fields' raw JSON into
// the recovered report.
var testFailuresMarkdownFieldRe = regexp.MustCompile(`(?s)"failures_markdown"\s*:\s*"(.*?)"\s*(?:,\s*"(?:surface_kind|app_started|start_command|readiness_probe|manual_probes|automated_checks|unable_to_run_reason)"\s*:|\s*\})`)

func extractFailuresMarkdownFieldRegex(s string) string {
	m := testFailuresMarkdownFieldRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return unescapeJSONStringBestEffort(m[1])
}

// unescapeJSONStringBestEffort decodes the common JSON string escapes
// (\n, \t, \r, \", \\) in text that couldn't be run through json.Unmarshal
// because the surrounding JSON was malformed. It is intentionally lenient:
// unrecognized escape sequences are left as-is rather than treated as errors.
func unescapeJSONStringBestEffort(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case '"':
				b.WriteByte('"')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func normalizeStructuredFailuresMarkdown(report, outcome string) string {
	report = strings.TrimSpace(report)
	if report == "" {
		return ""
	}
	report = stripTestVerdictMarkers(report)
	outcome = normalizeTestOutcome(outcome)
	if testFailSectionOf(report) == "" {
		prefix := testFailuresHeading + "\n\n"
		if outcome != "" {
			prefix += "Classification: " + outcome + "\n\n"
		}
		return prefix + report + "\n"
	}
	if outcome == "" || explicitTestOutcome(report) != "" {
		return report + "\n"
	}
	return insertClassificationAfterFailuresHeading(report, outcome) + "\n"
}

func hasManualPassEvidence(output string, t TaskInfo) (ok bool, reason string) {
	parsed, ok := parseStructuredTestOutput(output)
	if !ok {
		return hasPlainTextManualPassEvidence(output, t)
	}
	if strings.ToUpper(strings.TrimSpace(parsed.Verdict)) != "PASS" {
		return true, ""
	}
	if normalizeTestOutcome(parsed.Outcome) != testOutcomePass {
		return false, "PASS report outcome was not pass"
	}
	if strings.TrimSpace(parsed.FailuresMarkdown) != "" {
		return false, "PASS report included failures_markdown"
	}
	surface := normalizeSurfaceKind(parsed.SurfaceKind)
	if surface == "" {
		return false, "PASS report omitted surface_kind"
	}
	if isManualTestExemption(surface, t) {
		if strings.TrimSpace(parsed.UnableToRunReason) == "" {
			return false, "PASS used a no-app exemption but omitted unable_to_run_reason"
		}
		if !hasRegressionCheckEvidence(parsed.AutomatedChecks) &&
			!hasReadinessProbeEvidence(parsed.ReadinessProbe) &&
			!hasManualProbeEvidence(parsed.ManualProbes) {
			return false, "PASS used a no-app exemption without CLI/test harness evidence"
		}
		return true, ""
	}
	if surface == "library" || surface == "docs" || surface == "none" {
		return false, "PASS skipped manual testing without an explicit docs/library exemption"
	}
	if !parsed.AppStarted {
		return false, "PASS report did not confirm app_started"
	}
	if strings.TrimSpace(parsed.StartCommand) == "" {
		return false, "PASS report omitted start_command"
	}
	if strings.TrimSpace(parsed.ReadinessProbe.text()) == "" {
		return false, "PASS report omitted readiness_probe"
	}
	if !hasManualProbeEvidence(parsed.ManualProbes) {
		return false, "PASS report omitted user-facing manual probe evidence"
	}
	return true, ""
}

// surfaceNegationPattern strips clauses that explicitly DENY a runnable surface
// ("no HTTP/CLI/UI surface", "no runnable product surface") before the positive
// token scan. Without this, a tester who correctly states the change has no
// product surface is misclassified as owning the very surface it disclaims —
// e.g. "cli" pulled out of "(no HTTP/CLI/UI surface)".
var surfaceNegationPattern = regexp.MustCompile(
	`\bno\s+[a-z0-9/,\- ]*\bsurface\b|\bno\s+runnable\b|\bno\s+(?:(?:http|https|cli|ui|web|server|desktop|k8s|kubernetes|api|gui)\s*(?:(?:/|,|\bor\b|\band\b)\s*)?)+`,
)

// internalSurfaceMarkers identify an internal/library/no-product-surface change
// described in prose ("internal Go component/package", "background goroutine").
// They map to the "library" exemption — which still requires regression
// evidence — but only as a last resort when no positive product-surface token
// is present, so "internal Kubernetes informer" still resolves to its real k8s
// surface and keeps the app-start requirement.
var internalSurfaceMarkers = []string{
	"internal", "library", "package", "component", "refactor",
	"helper", "goroutine", "background", "no runnable",
	"no product surface", "no user-facing", "not a product surface",
}

func normalizeSurfaceKind(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch lower {
	case "web", "cli", "server", "desktop", "k8s", "library", "docs", "none":
		return lower
	}
	scan := surfaceNegationPattern.ReplaceAllString(lower, " ")
	var fallback string
	for _, token := range strings.FieldsFunc(scan, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		switch token {
		case "web", "ui", "browser", "sveltekit", "frontend":
			return "web"
		case "cli", "server", "desktop", "k8s":
			return token
		case "kubernetes":
			if fallback == "" {
				fallback = "k8s"
			}
		case "library", "docs", "none":
			if fallback == "" || fallback == "k8s" {
				fallback = token
			}
		}
	}
	if fallback == "" && containsAny(lower, internalSurfaceMarkers...) {
		return "library"
	}
	return fallback
}

func isManualTestExemption(surface string, t TaskInfo) bool {
	if surface == "library" {
		return true
	}
	if surface == "docs" {
		return taskHasAnyTag(t, "docs", "documentation", "notest")
	}
	if surface == "none" {
		return taskHasAnyTag(t, "notest")
	}
	return false
}

func hasPlainTextManualPassEvidence(output string, t TaskInfo) (ok bool, reason string) {
	surface := normalizeSurfaceKind(firstPlainEvidenceField(output, "surface_kind", "surface kind"))
	if surface == "" {
		return false, "plain-text PASS report omitted surface_kind"
	}
	if isManualTestExemption(surface, t) {
		if firstPlainEvidenceField(output, "unable_to_run_reason", "unable to run manual test") == "" {
			return false, "plain-text PASS used a no-app exemption but omitted unable_to_run_reason"
		}
		if !hasPlainTextRegressionCheckEvidence(output) {
			return false, "plain-text PASS used a no-app exemption without CLI/test harness evidence"
		}
		return true, ""
	}
	if surface == "library" || surface == "docs" || surface == "none" {
		return false, "plain-text PASS skipped manual testing without an explicit docs/library exemption"
	}
	lower := strings.ToLower(output)
	if !containsAny(lower, "app_started: true", "app started: true") {
		return false, "plain-text PASS report did not confirm app_started"
	}
	if firstPlainEvidenceField(output, "start_command", "start command") == "" {
		return false, "plain-text PASS report omitted start_command"
	}
	if firstPlainEvidenceField(output, "readiness_probe", "readiness probe") == "" {
		return false, "plain-text PASS report omitted readiness_probe"
	}
	if !hasPlainTextManualProbeEvidence(output) {
		return false, "plain-text PASS report omitted user-facing manual probe evidence"
	}
	return true, ""
}

func firstPlainEvidenceField(output string, names ...string) string {
	for _, line := range reportScanLines(output) {
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field = strings.Trim(strings.ToLower(field), "-* \t`")
		field = strings.ReplaceAll(field, " ", "_")
		for _, name := range names {
			normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "_")
			if field == normalized {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func hasPlainTextManualProbeEvidence(output string) bool {
	lower := strings.ToLower(output)
	return containsAny(lower, "manual_probes:", "manual probe:", "manual_probe:") &&
		containsAny(lower, "command:", "curl ", "sybra-cli", "kubectl ", "go run ", "npm run ") &&
		containsAny(lower, "expected:", "expected output:") &&
		containsAny(lower, "actual:", "actual output:", "observed:", "observed output:")
}

func hasPlainTextRegressionCheckEvidence(output string) bool {
	lower := strings.ToLower(output)
	return containsAny(lower, "automated_checks:", "automated checks:", "regression check:", "test harness:") &&
		containsAny(lower,
			"go test", "npm test", "npm run test", "npm run check",
			"pnpm test", "pnpm run test", "yarn test", "pytest", "cargo test",
			"sybra-cli", "go run", "curl ", "kubectl ") &&
		hasSuccessfulCheckResult(evidenceText(output))
}

func taskHasAnyTag(t TaskInfo, tags ...string) bool {
	for _, got := range t.Tags {
		for _, want := range tags {
			if strings.EqualFold(strings.TrimSpace(got), want) {
				return true
			}
		}
	}
	return false
}

func hasManualProbeEvidence(probes manualProbeEvidenceList) bool {
	for _, p := range probes {
		if strings.TrimSpace(p.Command) != "" &&
			(strings.TrimSpace(string(p.Actual)) != "" ||
				strings.TrimSpace(string(p.Observed)) != "" ||
				strings.TrimSpace(string(p.Output)) != "" ||
				strings.TrimSpace(string(p.Status)) != "") {
			return true
		}
		if hasRawManualProbeEvidence(p.Raw) {
			return true
		}
	}
	return false
}

func hasReadinessProbeEvidence(probe readinessProbeEvidence) bool {
	cmd := strings.ToLower(strings.TrimSpace(probe.Command))
	if cmd != "" {
		if hasRegressionCheckCommandEvidence(cmd) {
			return hasSuccessfulCheckResult(probe.Actual, probe.Output, probe.Observed, probe.Status)
		}
		if strings.TrimSpace(string(probe.Actual)) != "" ||
			strings.TrimSpace(string(probe.Output)) != "" ||
			strings.TrimSpace(string(probe.Observed)) != "" ||
			strings.TrimSpace(string(probe.Status)) != "" {
			return hasRawManualProbeEvidence(probe.text())
		}
	}
	return hasRawReadinessProbeEvidence(probe.Raw)
}

func hasRawReadinessProbeEvidence(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" || hasReadinessHypotheticalEvidence(lower) || !hasExecutedResultEvidence(lower) {
		return false
	}
	return hasRawRegressionCheckEvidence(lower) || hasRawManualProbeEvidence(lower)
}

func hasRegressionCheckEvidence(checks automatedCheckEvidenceList) bool {
	for _, c := range checks {
		cmd := strings.ToLower(strings.TrimSpace(c.Command))
		if cmd == "" || (strings.TrimSpace(string(c.Actual)) == "" &&
			strings.TrimSpace(string(c.Output)) == "" &&
			strings.TrimSpace(string(c.Observed)) == "" &&
			strings.TrimSpace(string(c.Status)) == "") {
			if hasRawRegressionCheckEvidence(c.Raw) {
				return true
			}
			continue
		}
		if hasRegressionCheckCommandEvidence(cmd) {
			return hasSuccessfulCheckResult(c.Actual, c.Output, c.Observed, c.Status)
		}
	}
	return false
}

func hasSuccessfulCheckResult(values ...evidenceText) bool {
	var parts []string
	hasNumericSuccess := false
	for _, value := range values {
		if s := strings.TrimSpace(string(value)); s != "" {
			if hasFailureCheckResult(s) {
				return false
			}
			hasNumericSuccess = hasNumericSuccess || isNumericSuccessResult(s)
			parts = append(parts, s)
		}
	}
	result := strings.ToLower(strings.Join(parts, " "))
	if result == "" {
		return false
	}
	if hasFailureCheckResult(result) {
		return false
	}
	if hasNumericSuccess || isNumericSuccessResult(result) {
		return true
	}
	return containsAny(result,
		"pass", "passed", "success", "true", "exit code 0",
		"exit status 0", "exit 0", "no matches", "200", "201", "202", "204", "created", "returned") ||
		checkOKPattern.MatchString(result)
}

var (
	checkFailureStatusPattern = regexp.MustCompile(`\b(exit (code|status)?|status)\s*:?\s*([1-9]\d*|[45]\d\d)\b`)
	checkFailureHTTPPattern   = regexp.MustCompile(`\b(?:http(?:/[0-9.]+)?\s*)?[45]\d\d\b`)
	checkFailureWordPattern   = regexp.MustCompile(`\b([1-9]\d*\s+(failed|failures?|failing|errors?)|(test|tests|command|check|checks)\s+(failed|failing|errored)|error:|failed:)\b`)
	checkOKPattern            = regexp.MustCompile(`\bok\b`)
)

func hasFailureCheckResult(result string) bool {
	lower := strings.ToLower(strings.TrimSpace(result))
	if lower == "" {
		return false
	}
	switch lower {
	case "false", "fail", "failed", "failure", "error", "not ok":
		return true
	}
	if isNumericFailureResult(lower) {
		return true
	}
	if containsAny(lower, "not ok", "panic:", "assertion failed") {
		return true
	}
	return checkFailureWordPattern.MatchString(lower) ||
		checkFailureStatusPattern.MatchString(lower) ||
		checkFailureHTTPPattern.MatchString(lower)
}

func isNumericFailureResult(result string) bool {
	n, err := strconv.ParseFloat(strings.TrimSpace(result), 64)
	if err != nil {
		return false
	}
	return n >= 400 || (n > 0 && n < 100)
}

func isNumericSuccessResult(result string) bool {
	n, err := strconv.ParseFloat(strings.TrimSpace(result), 64)
	if err != nil {
		return false
	}
	return n == 0 || (n >= 200 && n < 300)
}

func hasRawManualProbeEvidence(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return false
	}
	if hasManualNegativeEvidence(lower) {
		return false
	}
	return hasManualActionEvidence(lower) && hasManualObservationEvidence(lower)
}

func hasRawRegressionCheckEvidence(raw string) bool {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return false
	}
	return hasRegressionCheckCommandEvidence(lower) && hasPositiveRegressionEvidence(lower)
}

func hasExecutedResultEvidence(lower string) bool {
	return containsAny(lower,
		"->", "=>", "ran ", "ran:", "executed", "exit code", "exit status",
		"output:", "actual:", "observed:", "confirmed", "status:", "returned")
}

func hasReadinessHypotheticalEvidence(lower string) bool {
	if !containsAny(lower,
		"would ", "would've", "would have", "could ", "could've", "could have",
		"should ", "should've", "should have", "expected to", "if the ",
		"if it ", "if this ", "if run", "assuming ", "hypothetical") {
		return false
	}
	return !containsAny(lower,
		"->", "=>", "actual:", "observed:", "output:", "status:", "confirmed",
		"it returned", "command returned", "request returned", "endpoint returned",
		"got ", "received ")
}

func hasManualActionEvidence(lower string) bool {
	return containsAny(lower,
		"curl ", "sybra-cli", "kubectl ", "go run ", "npm run ",
		"get ", "post ", "put ", "patch ", "delete ", "head ", "options ",
		"http://", "https://") ||
		containsAny(lower, "click", "clicked", "opened", "visited", "navigated", "typed", "selected", "pressed", "submitted") &&
			containsAny(lower,
				" app", " ui", " page", " screen", " board", " card", " button", " action",
				" banner", " form", " field", " link", " menu", " route", " endpoint",
				" dialog", " modal", " row", " list", " panel", " tab", " workflow", " task")
}

func hasManualObservationEvidence(lower string) bool {
	return containsAny(lower,
		"->", "=>", "returned", "created", "observed", "actual",
		"showed", "visible", "absent", "present", "loaded", "closed",
		"exposes", "computed", "became", "changed",
		"verified", "displayed", "rendered", "confirmed")
}

func hasManualNegativeEvidence(lower string) bool {
	return containsAny(lower,
		"saw nothing", "nothing changed", "not visible", "not displayed", "not rendered",
		"did not", "does not", "could not", "failed to")
}

func hasRegressionCheckCommandEvidence(lower string) bool {
	return containsAny(lower,
		"go test", "npm test", "npm run test", "npm run check",
		"pnpm test", "pnpm run test", "yarn test", "pytest", "cargo test",
		"sybra-cli", "go run", "curl ", "kubectl ",
		"type-check", "typecheck", "lint", "unit test", "unit tests", "acceptance invariant")
}

func hasPositiveRegressionEvidence(lower string) bool {
	if hasRegressionNegativeEvidence(lower) {
		return false
	}
	return hasSuccessfulCheckResult(evidenceText(lower))
}

func hasRegressionNegativeEvidence(lower string) bool {
	return containsAny(lower,
		"not run", "not executed", "never ran", "never run", "did not run", "didn't run",
		"was not run", "were not run", "wasn't run", "weren't run", "could not run",
		"cannot run", "skipped", "without running", "pass assumed", "assumed pass",
		"did not pass", "-> fail", "=> fail", " failed", " failure")
}

func insertClassificationAfterFailuresHeading(report, outcome string) string {
	lines := strings.Split(report, "\n")
	for i, line := range lines {
		if isTestFailuresHeading(strings.TrimSpace(line)) {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, "", "Classification: "+outcome)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	return report
}

func appendRawBody(body, content string) string {
	body = strings.TrimRight(body, "\n")
	if body != "" {
		body += "\n\n"
	}
	return body + strings.TrimSpace(content) + "\n"
}

func stripTestVerdictMarkers(report string) string {
	lines := strings.Split(report, "\n")
	lastNonEmpty := -1
	lastNonEmptyInFence := false
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lastNonEmpty = i
			lastNonEmptyInFence = inFence
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
	}
	if lastNonEmpty >= 0 && !lastNonEmptyInFence {
		trimmed := strings.TrimSpace(lines[lastNonEmpty])
		if trimmed == testVerdictPass || trimmed == testVerdictFail {
			lines = append(lines[:lastNonEmpty], lines[lastNonEmpty+1:]...)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// applyTestVerdictCompletion extracts the machine verdict from run_test output,
// records protocol violations, and turns protocol-violating reports into failed
// steps. The run_agent max_retries budget then retries the tester instead of
// sending implementation agents after a bad report. It returns the violation
// name when one should be persisted on the underlying AgentRun, plus the typed
// outcome and failure fingerprint for attempt accounting.
func applyTestVerdictCompletion(wfExec *Execution, output *StepOutput, body string, t TaskInfo) (violation, outcome, fingerprint string) {
	if wfExec == nil || output == nil || output.StepID != testVerdictSourceStep {
		return "", "", ""
	}
	if output.Status != "completed" && output.Status != "failed" {
		return "", "", ""
	}
	v := ExtractTestVerdict(output.Output)
	outcome, fingerprint = classifyTestOutcome(output.Status, output.Output, body, wfExec, output.StepID)
	if outcome != "" {
		wfExec.SetVar("step."+output.StepID+"."+testVerdictOutcomeKey, outcome)
	}
	if fingerprint != "" {
		wfExec.SetVar("step."+output.StepID+"."+testFailureFingerprintKey, fingerprint)
	}
	if outcome == testOutcomeProtocolViolation {
		wfExec.SetVar("step."+output.StepID+"."+testVerdictTaintedKey, testProtocolFixSuggestions)
		output.Status = "failed"
		output.Output = appendTestProtocolViolation(output.Output, "FAIL report contained fix suggestions instead of observed symptoms")
		return testProtocolFixSuggestions, outcome, fingerprint
	}
	if outcome == testOutcomeMissingEvidence {
		wfExec.SetVar("step."+output.StepID+"."+testVerdictTaintedKey, testProtocolMissingEvidence)
		output.Status = "failed"
		output.Output = appendTestProtocolViolation(output.Output, "FAIL report lacked machine-checkable evidence")
		return testProtocolMissingEvidence, outcome, fingerprint
	}
	if output.Status == "completed" && outcome == testOutcomePass && v == "PASS" {
		if ok, reason := hasManualPassEvidence(output.Output, t); !ok {
			wfExec.SetVar("step."+output.StepID+"."+testVerdictOutcomeKey, testOutcomeMissingEvidence)
			wfExec.SetVar("step."+output.StepID+"."+testVerdictTaintedKey, testProtocolMissingEvidence)
			output.Status = "failed"
			output.Output = appendTestProtocolViolation(output.Output, reason)
			return testProtocolMissingEvidence, testOutcomeMissingEvidence, ""
		}
	}
	if outcome == testOutcomeInfraFailure {
		output.Status = "failed"
		output.Output = appendTestInfrastructureFailure(output.Output)
		return "", outcome, fingerprint
	}
	if output.Status != "completed" {
		return "", outcome, fingerprint
	}
	if v != "" {
		wfExec.SetVar("step."+output.StepID+".verdict", v)
	}
	return "", outcome, fingerprint
}

func appendTestProtocolViolation(output, detail string) string {
	msg := "test-runner protocol violation: " + detail
	if strings.TrimSpace(output) == "" {
		return msg
	}
	return strings.TrimRight(output, "\n") + "\n\n" + msg
}

func appendTestInfrastructureFailure(output string) string {
	msg := "test-runner infrastructure failure: runner exited without a parseable, evidenced verdict"
	if strings.TrimSpace(output) == "" {
		return msg
	}
	return strings.TrimRight(output, "\n") + "\n\n" + msg
}

func classifyTestOutcome(status, output, body string, wfExec *Execution, stepID string) (outcome, fingerprint string) {
	v := ExtractTestVerdict(output)
	if status == "failed" {
		return testOutcomeInfraFailure, ""
	}
	if (v == "" || v == "FAIL") && containsFixSuggestionsInCurrentTestReport(output, body, wfExec, stepID) {
		return testOutcomeProtocolViolation, ""
	}
	switch v {
	case "PASS":
		return testOutcomePass, ""
	case "FAIL":
	default:
		if currentTestFailureReport(output, body, wfExec, stepID) == "" {
			return testOutcomeInfraFailure, ""
		}
		return testOutcomeMissingEvidence, ""
	}

	report := currentTestFailureReport(output, body, wfExec, stepID)
	if strings.TrimSpace(report) == "" {
		return testOutcomeMissingEvidence, ""
	}

	// A well-formed structured verdict is authoritative about *classification*:
	// trust the tester's own `outcome` field instead of re-deriving product_bug
	// vs. ambiguous_requirement vs. missing_evidence from prose-shape heuristics
	// the tester's contract never guaranteed a specific markdown layout for.
	// This does NOT waive the evidence requirement itself — a tester still has
	// to show a real command and observed result, either as grounded prose
	// (hasGroundedFailureEvidence) or as populated structured evidence fields
	// (hasStructuredFailureEvidence: readiness_probe/manual_probes/
	// automated_checks). Without the grounded-prose fallback, a test-runner
	// that reports the identical {"verdict":"FAIL","outcome":"product_bug"}
	// with a real repro every retry, just formatted without a labeled
	// "Command:"/"Expected:" section or file:line citation, got bounced back as
	// missing_evidence and burned the whole auto-retry budget on a result the
	// first run already reported correctly (#1382). A bare narrative sentence
	// with no evidence anywhere still falls through to the evidence gate below.
	// Reconstructed reports (body-delta fallback, or a bare "Classification:"
	// line in prose) are NOT covered here — only an outcome parsed directly off
	// the agent's own structured output.
	if parsed, ok := parseStructuredTestOutput(output); ok &&
		strings.EqualFold(strings.TrimSpace(parsed.Verdict), "FAIL") &&
		strings.TrimSpace(parsed.FailuresMarkdown) != "" {
		switch normalizeTestOutcome(parsed.Outcome) {
		case testOutcomeProductBug, testOutcomeAmbiguousRequirement:
			if hasGroundedFailureEvidence(report) || hasStructuredFailureEvidence(parsed) {
				return normalizeTestOutcome(parsed.Outcome), testFailureFingerprint(report)
			}
		}
	}

	if explicit := explicitTestOutcome(report); explicit != "" {
		if explicit == testOutcomePass {
			return testOutcomeMissingEvidence, ""
		}
		if explicit == testOutcomeProductBug || explicit == testOutcomeAmbiguousRequirement {
			if !hasGroundedFailureEvidence(report) {
				return testOutcomeMissingEvidence, ""
			}
			return explicit, testFailureFingerprint(report)
		}
		return explicit, ""
	}
	if !hasGroundedFailureEvidence(report) {
		return testOutcomeMissingEvidence, ""
	}
	return testOutcomeProductBug, testFailureFingerprint(report)
}

func currentTestFailureReport(output, body string, wfExec *Execution, stepID string) string {
	if parsed, ok := parseStructuredTestOutput(output); ok {
		if report := normalizeStructuredFailuresMarkdown(parsed.FailuresMarkdown, parsed.Outcome); report != "" {
			return report
		}
	}
	if section := testFailSectionOf(output); section != "" {
		return section
	}
	delta, ok := testFailureBodyDelta(body, wfExec, stepID)
	if !ok || strings.TrimSpace(delta) == "" {
		return ""
	}
	if section := testFailSectionOf(delta); section != "" {
		return section
	}
	return delta
}

func explicitTestOutcome(report string) string {
	for _, line := range reportScanLines(report) {
		field, value, ok := strings.Cut(strings.ToLower(line), ":")
		if !ok {
			continue
		}
		field = strings.Trim(field, "-* \t")
		if field != "classification" && field != "class" && field != "type" && field != "outcome" {
			continue
		}
		if outcome := normalizeTestOutcome(value); outcome != "" {
			return outcome
		}
	}
	return ""
}

func normalizeTestOutcome(s string) string {
	token := strings.Trim(strings.ToLower(s), " .;,-_*`\"'")
	token = strings.NewReplacer("-", "_", " ", "_", ":", "_", ",", "_").Replace(token)
	token = strings.Trim(token, "_")
	switch {
	case outcomeTokenStarts(token, testOutcomePass):
		return testOutcomePass
	case outcomeTokenStarts(token, testOutcomeProductBug, "bug", "product_failure"):
		return testOutcomeProductBug
	case outcomeTokenStarts(token, testOutcomeInfraFailure, "infrastructure_failure", "infrastructure", "infra"):
		return testOutcomeInfraFailure
	case outcomeTokenStarts(token, testOutcomeMissingEvidence, "ungrounded", "no_evidence"):
		return testOutcomeMissingEvidence
	case outcomeTokenStarts(token, testOutcomeAmbiguousRequirement, "ambiguous", "spec_ambiguity", "ambiguous_spec"):
		return testOutcomeAmbiguousRequirement
	case outcomeTokenStarts(token, testOutcomeProtocolViolation, "test_protocol_violation", "protocol"):
		return testOutcomeProtocolViolation
	default:
		return ""
	}
}

func outcomeTokenStarts(token string, candidates ...string) bool {
	for _, candidate := range candidates {
		if token == candidate || strings.HasPrefix(token, candidate+"_") {
			return true
		}
	}
	return false
}

func reportScanLines(text string) []string {
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
		out = append(out, trimmed)
	}
	return out
}

// hasStructuredFailureEvidence reports whether a structured FAIL payload's own
// evidence fields (readiness_probe/manual_probes/automated_checks) record a
// real reproduction, as an alternative to hasGroundedFailureEvidence's prose
// scan. It lets a tester that fills these fields correctly skip the markdown
// labeling hasGroundedFailureEvidence expects, without waiving evidence
// entirely (see the classifyTestOutcome comment for the incident this covers).
func hasStructuredFailureEvidence(parsed structuredTestOutput) bool {
	for _, p := range parsed.ManualProbes {
		if hasConcreteProbeEvidence(p.Command, p.Actual, p.Output, p.Observed, p.Status, p.Raw) {
			return true
		}
	}
	for _, c := range parsed.AutomatedChecks {
		if hasConcreteProbeEvidence(c.Command, c.Actual, c.Output, c.Observed, c.Status, c.Raw) {
			return true
		}
	}
	rp := parsed.ReadinessProbe
	return hasConcreteProbeEvidence(rp.Command, rp.Actual, rp.Output, rp.Observed, rp.Status, rp.Raw)
}

// hasConcreteProbeEvidence reports whether a structured probe records a real
// reproduction: a command plus at least one populated result field, or a
// free-form raw string with both a stated action and an observed result.
// Unlike hasSuccessfulCheckResult (built to confirm a PASS), this makes no
// success/failure judgment on the result — a FAIL report showing the probe
// actually ran and what it returned is exactly the evidence we want.
func hasConcreteProbeEvidence(command string, actual, output, observed, status evidenceText, raw string) bool {
	if strings.TrimSpace(command) != "" &&
		(hasConcreteProbeResult(actual) ||
			hasConcreteProbeResult(output) ||
			hasConcreteProbeResult(observed) ||
			hasConcreteProbeResult(status)) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if lower == "" {
		return false
	}
	return hasManualActionEvidence(lower) && hasManualObservationEvidence(lower)
}

// hasConcreteProbeResult reports whether a probe result field (actual/output/
// observed/status) records a real outcome rather than a not-executed marker
// (e.g. "not run", "skipped", "could not run"). Applied uniformly to all four
// fields because fillAliases backfills an empty Actual from Status, so a
// negative status would otherwise resurface as "concrete" Actual content and
// let an admittedly-unexecuted probe pass as reproduction evidence. Unlike
// hasRegressionNegativeEvidence, this must NOT flag "failed"/"failure" — a
// probe result of "failed" means it ran and failed, which is exactly the
// reproduction evidence this gate wants.
func hasConcreteProbeResult(v evidenceText) bool {
	trimmed := strings.TrimSpace(string(v))
	if trimmed == "" {
		return false
	}
	return !containsAny(strings.ToLower(trimmed),
		"not run", "not executed", "never ran", "never run", "did not run", "didn't run",
		"was not run", "were not run", "wasn't run", "weren't run", "could not run",
		"cannot run", "skipped", "without running", "n/a", "unknown")
}

func hasGroundedFailureEvidence(report string) bool {
	lower := strings.ToLower(report)
	// Header-style keywords ("command:", "actual:", "expected:", "output:", ...)
	// are routed through hasLabeledSection rather than a plain substring check:
	// a bare header with nothing after the colon (e.g. a report that stacks
	// "**Command:**\n**Actual:**\n**Expected:**" with no real content) must not
	// satisfy the gate. Only phrases that are inherently content-bearing on
	// their own (a shell invocation, a stated exit code) stay as plain
	// substring checks.
	hasCommand := containsAny(lower,
		"go test ", "npm run ", "pnpm ", "yarn ", "curl ", "rg ", "grep ") ||
		hasLabeledSection(report,
			"command run", "command", "reproduction steps", "repro", "steps")
	hasObserved := containsAny(lower, "exit code") ||
		hasLabeledSection(report,
			"actual output", "actual", "observed output", "observed",
			"command output", "stdout", "stderr", "output",
			"verbatim output", "actual behavior", "actual behaviour",
			"observed behavior", "observed behaviour", "printed", "rendered",
			// Natural-language observed-evidence headers whose core keyword above
			// isn't present as a standalone word (e.g. "what happened", "what i
			// saw"). hasLabeledSection now matches keywords anywhere in a header
			// label (#1386), so headers like "**What actually happened:**" are
			// already covered by "actual"/"output"; these catch the phrasings that
			// carry no core keyword at all.
			"what actually happened", "what happened", "what i observed",
			"what i saw", "what the code shows", "what the code does")
	hasExpected := containsAny(lower,
		"task says", "task requires", "from the task", "violates",
		"should render", "should not") ||
		hasLabeledSection(report,
			"expected", "expected output", "expected behavior", "expected behaviour",
			"requirement tested", "task expectation", "task requirement")
	hasGrounding := containsAny(lower,
		"quoted code", "current source", "current file", "source quote") ||
		fileLineCitationRe.MatchString(report) ||
		hasLabeledSection(report,
			"code evidence", "current code line evidence", "code line evidence")
	return hasCommand && hasObserved && hasExpected && hasGrounding
}

// evidenceLabelRe matches the label portion (the text before the colon) of a
// header-like line: a short token of letters, digits and a little punctuation.
// It keeps keyword matching scoped to real headers instead of arbitrary prose
// that merely contains a colon (which would carry a comma, longer text, etc.).
var evidenceLabelRe = regexp.MustCompile(`^[a-z][a-z0-9 '/()-]{0,59}$`)

// headerLikeLineRe matches a report line that STARTS with a short
// markdown-decorated label followed by a colon, whether or not further text
// follows on the same line, e.g. "actual:** something happened" or
// "command:". Used to recognize that a line belongs to a *different* header's
// own section rather than being prose content continuing a preceding bare
// header — a bare header immediately followed by another header's line (with
// or without inline content) still has no content of its own. Its label
// length ceiling must stay in sync with evidenceLabelRe's, otherwise a bare
// header can borrow a longer header's content as its own (#1386 follow-up).
var headerLikeLineRe = regexp.MustCompile(`^[a-z][a-z0-9 '/()-]{0,59}:`)

// labelMatchesKeyword reports whether a header label matches any keyword.
// Single-word keywords must match at the label's START (as a whole word), so a
// generic word like "output" is not credited inside a different-category label
// such as "expected output". Multi-word phrase keywords ("actual output",
// "what actually happened") may match ANYWHERE in the label — this is what lets
// natural mid-phrase headers satisfy the gate (#1386) without collapsing the
// observed/expected distinction.
func labelMatchesKeyword(label string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(kw, " ") {
			if labelHasKeyword(label, kw) {
				return true
			}
		} else if labelHasPrefixKeyword(label, kw) {
			return true
		}
	}
	return false
}

// labelHasPrefixKeyword reports whether label begins with kw as a whole word
// (kw is the entire label, or is immediately followed by a non-alphanumeric
// byte). label is expected to be already lower-cased.
func labelHasPrefixKeyword(label, kw string) bool {
	if !strings.HasPrefix(label, kw) {
		return false
	}
	return len(label) == len(kw) || !isAlnumByte(label[len(kw)])
}

// labelHasKeyword reports whether kw occurs in label as a whole word/phrase,
// bounded by non-alphanumeric characters. This lets "actual output" match
// mid-label (e.g. "runtime actual output") while "actual" does not match inside
// "actually". label is expected to be already lower-cased.
func labelHasKeyword(label, kw string) bool {
	if kw == "" {
		return false
	}
	for start := 0; start <= len(label); {
		rel := strings.Index(label[start:], kw)
		if rel < 0 {
			return false
		}
		i := start + rel
		beforeOK := i == 0 || !isAlnumByte(label[i-1])
		afterOK := i+len(kw) >= len(label) || !isAlnumByte(label[i+len(kw)])
		if beforeOK && afterOK {
			return true
		}
		start = i + 1
	}
	return false
}

func isAlnumByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// fileLineCitationRe matches a real file:line citation such as
// "internal/x.go:42" or "src/App.svelte:10" — a path segment ending in a
// known source extension, followed by a line number. Unlike a bare "src/" or
// "internal/" substring (which can appear incidentally, e.g. inside a `go
// test ./internal/workflow/ ...` command line), this requires the extension
// AND line number together, so it can't be satisfied by quoting a package
// path with no actual code citation.
var fileLineCitationRe = regexp.MustCompile(`[\w./-]+\.(?:go|ts|tsx|js|jsx|svelte):\d+`)

// hasLabeledSection reports whether any report line is a markdown section header
// whose label starts with one of keywords, optionally followed by a parenthetical
// qualifier, then a colon — e.g. "**Expected (task's own words):**" — AND that
// header is backed by real evidence: either inline text after the colon, or a
// fenced code block / non-header line immediately following it. Leading markdown
// decoration (*, _, >, #, -) is stripped before matching. This keeps
// hasGroundedFailureEvidence from rejecting grounded FAIL reports that annotate
// their evidence headers, while still rejecting reports that stack empty headers
// with no real content to game the gate.
func hasLabeledSection(report string, keywords ...string) bool {
	rawLines := strings.Split(report, "\n")
	inFence := false
	for i, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		lower := strings.ToLower(trimmed)
		lower = strings.TrimLeft(lower, "*_>#- \t")
		// A header-like line is "<label>: <content>". Match keywords anywhere in
		// the label (word-boundary aware) rather than only as its first token, so
		// natural headers whose evidence keyword is mid-phrase — e.g. "Here is the
		// actual output:" — are still recognized (#1386). The label must be a
		// short header-like token and the header must be backed by real content,
		// which preserves the bare-header rejections.
		before, after, found := strings.Cut(lower, ":")
		if !found {
			continue
		}
		label := strings.TrimRight(strings.TrimSpace(before), "*_ ")
		if p := strings.LastIndex(label, "("); p >= 0 && strings.HasSuffix(label, ")") {
			label = strings.TrimSpace(label[:p])
		}
		if !evidenceLabelRe.MatchString(label) {
			continue
		}
		if !labelMatchesKeyword(label, keywords) {
			continue
		}
		afterColon := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(after), "*_"))
		if afterColon != "" || hasFollowingContent(rawLines, i) {
			return true
		}
	}
	return false
}

// hasFollowingContent reports whether the bare label line at rawLines[idx] (a
// header with nothing after its colon) is backed by real evidence on a
// following line — a fenced code block, or any non-blank line that is not
// itself a header line (bare or otherwise). It stops at the first non-blank
// line: a header immediately followed by another header — whether that
// header itself carries inline content or not — has no content of its own,
// since the inline content belongs to the *other* header's section, not the
// bare one being tested.
func hasFollowingContent(rawLines []string, idx int) bool {
	for j := idx + 1; j < len(rawLines); j++ {
		trimmed := strings.TrimSpace(rawLines[j])
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			content, end := fenceContentEnd(rawLines, j+1)
			if content {
				return true
			}
			j = end
			continue
		}
		stripped := strings.TrimLeft(strings.ToLower(trimmed), "*_>#- \t")
		return !headerLikeLineRe.MatchString(stripped)
	}
	return false
}

// fenceContentEnd scans a fenced code block starting at rawLines[start] (the
// line immediately after the opening fence marker) and reports whether the
// block contains any non-blank line before its closing fence, plus the index
// of that closing fence marker (or the last line, if the fence never closes).
// An empty fence must not count as evidence backing a bare header.
func fenceContentEnd(rawLines []string, start int) (hasContent bool, end int) {
	for j := start; j < len(rawLines); j++ {
		trimmed := strings.TrimSpace(rawLines[j])
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			return hasContent, j
		}
		if trimmed != "" {
			hasContent = true
		}
	}
	return hasContent, len(rawLines) - 1
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func hasReportLinePrefix(report string, prefixes ...string) bool {
	normalizedPrefixes := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix = strings.ToLower(strings.TrimSpace(prefix)); prefix != "" {
			normalizedPrefixes = append(normalizedPrefixes, prefix)
		}
	}
	for _, line := range reportScanLines(report) {
		lower := strings.ToLower(strings.TrimSpace(line))
		for _, prefix := range normalizedPrefixes {
			if strings.HasPrefix(lower, prefix) {
				return true
			}
		}
	}
	return false
}

func testFailureFingerprint(report string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(report)), " ")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// containsFixSuggestionsInCurrentTestReport scans the agent result and the task
// body text added during the current run_test attempt. The body delta keeps
// compatibility with older Codex output_schema runs that returned only
// {"verdict":"FAIL"}.
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
	inFence := false
	offset := 0
	for _, line := range strings.SplitAfter(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			offset += len(line)
			continue
		}
		if !inFence && isTestFailuresHeading(trimmed) {
			return output[offset:]
		}
		offset += len(line)
	}
	return ""
}

func isTestFailuresHeading(line string) bool {
	if !strings.HasPrefix(strings.ToLower(line), strings.ToLower(testFailuresHeading)) {
		return false
	}
	if len(line) == len(testFailuresHeading) {
		return true
	}
	suffix := line[len(testFailuresHeading):]
	return strings.HasPrefix(suffix, "(") || strings.HasPrefix(suffix, " (")
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

// fixSuggestionPhrases are case-insensitive substrings that signal the test
// runner prescribed a code change rather than described observed behavior.
// False positives are acceptable — the taint is advisory and never changes the
// FAIL verdict or adds an extra re-implementation loop.
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

// ExtractTestVerdict returns "PASS"/"FAIL"/"" from agent output.
//
// Object-shaped output (leading `{` after trimming BOM/whitespace) is treated
// as authoritative JSON: the `verdict` field is parsed and the marker scan is
// skipped entirely. A malformed or unexpected object yields "", and callers
// interpret that empty verdict in the fail-safe direction, without falling
// through to the marker. That prevents a JSON body that incidentally contains
// a marker-shaped substring from misrouting. This is the path codex takes when
// --output-schema enforces a structured response.
//
// Non-object-shaped output (claude plain text) falls to the exact-line marker
// scan. The last matching line wins; missing/ambiguous output yields "", which
// callers treat as a non-pass/failing-safe verdict.
func ExtractTestVerdict(output string) string {
	// Strip a leading UTF-8 BOM, then trim whitespace before shape detection.
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if strings.HasPrefix(s, "{") {
		// Object-shaped: JSON is authoritative. No fall-through to marker scan.
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
			return ""
		}
		// JSON parse failed (e.g. an unescaped quote inside failures_markdown).
		// The corruption is almost never in the verdict field itself, so
		// regex-extract it directly rather than discarding a confirmed verdict.
		return extractVerdictFieldRegex(s)
	}
	if parsed, ok := parseStructuredTestOutput(s); ok {
		switch strings.ToUpper(strings.TrimSpace(parsed.Verdict)) {
		case "PASS":
			return "PASS"
		case "FAIL":
			return "FAIL"
		}
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
//   - pass        → ready-pr (a separate workflow opens the PR)
//   - product bug → in-progress with the latest grounded repro, until the
//     distinct-defect cap, then human-required for targeted local reproduction.
//   - tester/provisioning failures and ambiguous specs do not consume the
//     implementation retry budget.
//
// Counting prior test-runner runs (which persist on the task across the
// implement→review→test loop) gives a natural, stateless attempt counter:
// the just-finished run is already recorded, so the Nth failure sees N runs.
func (e *Engine) execRouteTestResult(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Read the untruncated verdict/outcome vars (set in engine_advance from the
	// full agent output and current body delta). Missing or infrastructure-shaped
	// outcomes fail closed, but do not burn implementation attempts.
	// The reask note (if any) was consumed by the just-completed run_test prompt;
	// drop it so it never bleeds into a later, unrelated testing cycle.
	delete(wfExec.Variables, testingReaskNoteVar)

	if wfExec.Variables["step."+testVerdictSourceStep+".verdict"] == "PASS" {
		if err := e.tasks.UpdateTaskStatus(taskID, "ready-pr", "manual testing passed"); err != nil {
			return StepOutput{}, err
		}
		e.logger.Info("workflow.test.passed", "task_id", taskID, "step", step.ID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "pass"}, nil
	}

	if violation := wfExec.Variables["step."+testVerdictSourceStep+"."+testVerdictTaintedKey]; violation != "" {
		// A missing-evidence report describes the runner, not the code — re-ask
		// the tester (with the specific evidence it owes) before a human. A
		// fix-suggestions violation already had one in-step retry and rarely
		// improves on re-ask, so it still escalates immediately.
		if violation == testProtocolMissingEvidence {
			return e.retryOrEscalateTransient(taskID, step.ID, testOutcomeMissingEvidence, missingEvidenceReask,
				"test-runner report lacked machine-checkable evidence after auto-retries — needs local reproduction",
				"protocol violation: "+violation, "workflow.test.protocol-violation", wfExec, t)
		}
		reason := "test-runner report violated testing protocol after retry: contained fix suggestions instead of observed symptoms"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.protocol-violation", "task_id", taskID, "violation", violation)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "protocol violation: " + violation}, nil
	}
	outcome := wfExec.Variables["step."+testVerdictSourceStep+"."+testVerdictOutcomeKey]
	switch outcome {
	case testOutcomeInfraFailure:
		return e.retryOrEscalateTransient(taskID, step.ID, testOutcomeInfraFailure, "",
			"testing infrastructure failed after auto-retries — no implementation attempt consumed; rerun testing or inspect the test-runner log",
			"infra failure", "workflow.test.infra-failure", wfExec, t)
	case testOutcomeMissingEvidence:
		return e.retryOrEscalateTransient(taskID, step.ID, testOutcomeMissingEvidence, missingEvidenceReask,
			"test-runner failed without grounded evidence after auto-retries — needs local reproduction before implementation retries",
			"missing evidence", "workflow.test.missing-evidence", wfExec, t)
	case testOutcomeAmbiguousRequirement:
		reason := "testing found ambiguous or contradictory requirements — human decision needed; see latest ## Test Failures"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.ambiguous-requirement", "task_id", taskID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "ambiguous requirement"}, nil
	}

	// Count test-runner runs that belong to the current testing cycle.
	// When a human re-dispatches after human-required, TestingCycleStartedAt is
	// set to the re-dispatch time; runs before that time are from prior cycles
	// and must not inflate the counter. Nil means no re-dispatch has happened
	// (first cycle or automatic implement→test loop), so all runs count.
	attempts, duplicate := e.countValidProductTestAttempts(t, wfExec)
	limit := e.maxTestAttempts
	if limit <= 0 {
		limit = defaultTestAttempts
	}

	if duplicate {
		reason := "same grounded test failure reproduced twice — needs targeted local reproduction/fix from latest ## Test Failures"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.duplicate-failure", "task_id", taskID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "duplicate failure"}, nil
	}

	if attempts >= limit {
		reason := fmt.Sprintf("manual testing found %d grounded product defects: feature still does not match the task — needs targeted local reproduction/fix from latest ## Test Failures", attempts)
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
	reason := "manual testing found a grounded product defect — re-implementing the latest ## Test Failures repro"
	if err := e.tasks.UpdateTaskStatus(taskID, "in-progress", reason); err != nil {
		return StepOutput{}, err
	}
	e.logger.Info("workflow.test.reimplement", "task_id", taskID, "attempts", attempts, "cap", limit)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "reimplement"}, nil
}

func (e *Engine) countValidProductTestAttempts(t TaskInfo, wfExec *Execution) (int, bool) {
	currentFingerprint := wfExec.Variables["step."+testVerdictSourceStep+"."+testFailureFingerprintKey]
	attempts := 0
	lastMatchingFailure := -1
	currentMatchingFailure := -1
	for i := range t.AgentRuns {
		run := t.AgentRuns[i]
		if t.TestingCycleStartedAt != nil && run.StartedAt.Before(*t.TestingCycleStartedAt) {
			continue
		}
		if run.Role != testRunnerRole {
			continue
		}
		if run.ProtocolViolation != "" {
			continue
		}
		switch run.TestOutcome {
		case testOutcomeProductBug:
			if run.TestFailureFingerprint == "" {
				continue
			}
			attempts++
			if currentFingerprint != "" && run.TestFailureFingerprint == currentFingerprint {
				lastMatchingFailure = currentMatchingFailure
				currentMatchingFailure = i
			}
		default:
			continue
		}
	}
	return attempts, currentMatchingFailure >= 0 &&
		lastMatchingFailure >= 0 &&
		hasInterveningCodeAuthorRun(t.AgentRuns, lastMatchingFailure, currentMatchingFailure)
}

func hasInterveningCodeAuthorRun(runs []AgentRunInfo, prev, current int) bool {
	if prev < 0 || current <= prev || current >= len(runs) {
		return false
	}
	for i := prev + 1; i < current; i++ {
		if isCodeAuthorRun(runs[i]) {
			return true
		}
	}
	return false
}

func isCodeAuthorRun(run AgentRunInfo) bool {
	switch run.Role {
	case "", "implementation", "fix-review", "pr-fix":
		return true
	default:
		return false
	}
}
