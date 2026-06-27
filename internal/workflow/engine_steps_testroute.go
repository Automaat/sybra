package workflow

import (
	"crypto/sha256"
	"encoding/hex"
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
	// older structured-output runs returned only {"verdict":"FAIL"}, so the
	// actual failure text had to be read from the body delta written by the
	// runner.
	testFailureBodyStartLenKey = "body_start_len"
	testVerdictOutcomeKey      = "outcome"
	testFailureFingerprintKey  = "failure_fingerprint"
	testFailuresHeading        = "## Test Failures"

	testOutcomePass                 = "pass"
	testOutcomeProductBug           = "product_bug"
	testOutcomeAmbiguousRequirement = "ambiguous_requirement"
	testOutcomeInfraFailure         = "infra_failure"
	testOutcomeMissingEvidence      = "missing_evidence"
	testOutcomeProtocolViolation    = "protocol_violation"

	testProtocolFixSuggestions  = "fix-suggestions"
	testProtocolMissingEvidence = "missing-evidence"
)

type structuredTestOutput struct {
	Verdict           string                   `json:"verdict"`
	Outcome           string                   `json:"outcome,omitempty"`
	FailuresMarkdown  string                   `json:"failures_markdown,omitempty"`
	SurfaceKind       string                   `json:"surface_kind,omitempty"`
	AppStarted        bool                     `json:"app_started,omitempty"`
	StartCommand      string                   `json:"start_command,omitempty"`
	ReadinessProbe    string                   `json:"readiness_probe,omitempty"`
	ManualProbes      []manualProbeEvidence    `json:"manual_probes,omitempty"`
	AutomatedChecks   []automatedCheckEvidence `json:"automated_checks,omitempty"`
	UnableToRunReason string                   `json:"unable_to_run_reason,omitempty"`
}

type manualProbeEvidence struct {
	Command  string `json:"command"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

type automatedCheckEvidence struct {
	Command string `json:"command"`
	Actual  string `json:"actual"`
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
	if parsed, ok := parseStructuredTestOutput(output.Output); ok {
		if strings.ToUpper(strings.TrimSpace(parsed.Verdict)) != "FAIL" {
			return false, body, nil
		}
		report = normalizeStructuredFailuresMarkdown(parsed.FailuresMarkdown, parsed.Outcome)
	} else if extractTestVerdict(output.Output) == "FAIL" {
		report = plainTestFailureReport(output.Output)
	}
	if report == "" {
		return false, body, nil
	}
	if delta, hasDelta := testFailureBodyDelta(body, wfExec, output.StepID); hasDelta && testFailSectionOf(delta) != "" {
		return false, body, nil
	}
	if err := e.tasks.AppendTaskBody(taskID, report); err != nil {
		return false, body, fmt.Errorf("append test failure report: %w", err)
	}
	return true, appendRawBody(body, report), nil
}

func plainTestFailureReport(output string) string {
	section := testFailSectionOf(output)
	if section == "" {
		return ""
	}
	return stripTestVerdictMarkers(section)
}

func parseStructuredTestOutput(output string) (structuredTestOutput, bool) {
	s := strings.TrimSpace(strings.TrimPrefix(output, "\xef\xbb\xbf"))
	if !strings.HasPrefix(s, "{") {
		return structuredTestOutput{}, false
	}
	var parsed structuredTestOutput
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return structuredTestOutput{}, false
	}
	return parsed, true
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
		if !hasRegressionCheckEvidence(parsed.AutomatedChecks) {
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
	if strings.TrimSpace(parsed.ReadinessProbe) == "" {
		return false, "PASS report omitted readiness_probe"
	}
	if !hasManualProbeEvidence(parsed.ManualProbes) {
		return false, "PASS report omitted user-facing manual probe evidence"
	}
	return true, ""
}

func normalizeSurfaceKind(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "web", "cli", "server", "desktop", "k8s", "library", "docs", "none":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return ""
	}
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
			"sybra-cli", "go run", "curl ", "kubectl ")
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

func hasManualProbeEvidence(probes []manualProbeEvidence) bool {
	for _, p := range probes {
		if strings.TrimSpace(p.Command) != "" &&
			strings.TrimSpace(p.Expected) != "" &&
			strings.TrimSpace(p.Actual) != "" {
			return true
		}
	}
	return false
}

func hasRegressionCheckEvidence(checks []automatedCheckEvidence) bool {
	for _, c := range checks {
		cmd := strings.ToLower(strings.TrimSpace(c.Command))
		if cmd == "" || strings.TrimSpace(c.Actual) == "" {
			continue
		}
		if containsAny(cmd,
			"go test", "npm test", "npm run test", "npm run check",
			"pnpm test", "pnpm run test", "yarn test", "pytest", "cargo test",
			"sybra-cli", "go run", "curl ", "kubectl ") {
			return true
		}
	}
	return false
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
	v := extractTestVerdict(output.Output)
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
	v := extractTestVerdict(output)
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
	if explicit := explicitTestOutcome(report); explicit != "" {
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

func hasGroundedFailureEvidence(report string) bool {
	lower := strings.ToLower(report)
	hasCommand := containsAny(lower,
		"command run:", "command:", "reproduction steps:", "repro:", "steps:",
		"go test ", "npm run ", "pnpm ", "yarn ", "curl ", "rg ", "grep ")
	hasObserved := containsAny(lower,
		"actual output:", "actual:", "observed:", "observed output:",
		"command output:", "stdout:", "stderr:", "exit code",
		"printed:", "rendered:") || hasReportLinePrefix(report, "output:")
	hasExpected := containsAny(lower,
		"expected:", "expected output:", "requirement tested:", "task says", "from the task",
		"violates", "should render", "should not")
	hasGrounding := containsAny(lower,
		"code evidence:", "quoted code", "current source", "src/", "internal/",
		".go:", ".ts:", ".tsx:", ".svelte:", ".js:", ".jsx:")
	return hasCommand && hasObserved && hasExpected && hasGrounding
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
	if wfExec.Variables["step."+testVerdictSourceStep+".verdict"] == "PASS" {
		if err := e.tasks.UpdateTaskStatus(taskID, "ready-pr", "manual testing passed"); err != nil {
			return StepOutput{}, err
		}
		e.logger.Info("workflow.test.passed", "task_id", taskID, "step", step.ID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "pass"}, nil
	}

	if violation := wfExec.Variables["step."+testVerdictSourceStep+"."+testVerdictTaintedKey]; violation != "" {
		reason := "test-runner report violated testing protocol after retry: contained fix suggestions instead of observed symptoms"
		if violation == testProtocolMissingEvidence {
			reason = "test-runner report violated testing protocol after retry: missing machine-checkable evidence"
		}
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.protocol-violation", "task_id", taskID, "violation", violation)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "protocol violation: " + violation}, nil
	}
	outcome := wfExec.Variables["step."+testVerdictSourceStep+"."+testVerdictOutcomeKey]
	switch outcome {
	case testOutcomeInfraFailure:
		reason := "testing infrastructure failed after retry — no implementation attempt consumed; rerun testing or inspect the test-runner log"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.infra-failure", "task_id", taskID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "infra failure"}, nil
	case testOutcomeMissingEvidence:
		reason := "test-runner failed without grounded evidence after retry — needs local reproduction before implementation retries"
		if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
			return StepOutput{}, err
		}
		e.logger.Warn("workflow.test.missing-evidence", "task_id", taskID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "missing evidence"}, nil
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
