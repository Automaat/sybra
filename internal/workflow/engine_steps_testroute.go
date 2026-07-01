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
	for _, candidate := range structuredTestOutputCandidates(output) {
		var parsed structuredTestOutput
		if err := json.Unmarshal([]byte(candidate), &parsed); err != nil {
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
	return fencedCodeBlockCandidates(s)
}

func fencedCodeBlockCandidates(output string) []string {
	var candidates []string
	var block strings.Builder
	inFence := false
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				if candidate := strings.TrimSpace(block.String()); candidate != "" {
					candidates = append(candidates, candidate)
				}
				block.Reset()
				inFence = false
				continue
			}
			inFence = true
			block.Reset()
			continue
		}
		if inFence {
			block.WriteString(line)
			block.WriteByte('\n')
		}
	}
	return candidates
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
			"observed behavior", "observed behaviour", "printed", "rendered")
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

// headerLikeLineRe matches a report line that STARTS with a short
// markdown-decorated label followed by a colon, whether or not further text
// follows on the same line, e.g. "actual:** something happened" or
// "command:". Used to recognize that a line belongs to a *different* header's
// own section rather than being prose content continuing a preceding bare
// header — a bare header immediately followed by another header's line (with
// or without inline content) still has no content of its own.
var headerLikeLineRe = regexp.MustCompile(`^[a-z][a-z0-9 '/()-]{0,40}:`)

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
		for _, kw := range keywords {
			rest, ok := strings.CutPrefix(lower, kw)
			if !ok {
				continue
			}
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, "(") {
				if idx := strings.Index(rest, ")"); idx >= 0 {
					rest = strings.TrimSpace(rest[idx+1:])
				}
			}
			if !strings.HasPrefix(rest, ":") {
				continue
			}
			afterColon := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(rest[1:]), "*_"))
			if afterColon != "" || hasFollowingContent(rawLines, i) {
				return true
			}
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
