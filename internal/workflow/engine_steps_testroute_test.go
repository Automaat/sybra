package workflow

import (
	"encoding/json"
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
		// Non-object shapes fall through to the marker scan (safe direction),
		// except a fenced JSON final report with a verdict is accepted for Claude.
		{"json_array_not_object", `[{"verdict":"PASS"}]`, ""},
		{"fenced_json_no_marker", "```json\n{\"verdict\":\"PASS\"}\n```", "PASS"},
		{"prose_then_fenced_json", "All probes passed.\n\n```json\n{\"verdict\":\"PASS\"}\n```", "PASS"},
		// app_started emitted as a quoted string must not break the verdict parse.
		{"json_string_app_started", `{"verdict":"PASS","app_started":"true"}`, "PASS"},
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

func TestClassifyTestOutcome_RecognizesParenthesizedFailureHeadingInBodyDelta(t *testing.T) {
	t.Parallel()

	initialBody := "## Problem\nExercise the testing gate."
	body := initialBody + "\n\n## Test Failures (round 5)\n\n" +
		"### Acceptance probe\n\n" +
		"Classification: product_bug.\n\n" +
		"Command run:\n```sh\nrg -n \"rawDate\" src/routes -g '*.svelte'\n```\n\n" +
		"Output:\n```text\nsrc/routes/page.svelte:42:{rawDate}\n```\n\n" +
		"Expected: task says dates render through the shared formatter.\n\n" +
		"Code evidence:\n```text\nsrc/routes/page.svelte:42:{rawDate}\n```\n"
	wf := &Execution{Variables: map[string]string{}}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)

	got, fingerprint := classifyTestOutcome("completed", "TEST_VERDICT: FAIL", body, wf, testVerdictSourceStep)
	if got != testOutcomeProductBug {
		t.Fatalf("outcome = %q, want %q", got, testOutcomeProductBug)
	}
	if fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
}

func TestStructuredFailureOutcomeInsertedAfterParenthesizedHeading(t *testing.T) {
	t.Parallel()

	report := "## Test Failures (round 5)\n\n" +
		"Command run:\n```sh\ncurl /status\n```\n\n" +
		"Output:\n```text\nconnection refused\n```\n\n" +
		"Expected: task says the service should be reachable.\n\n" +
		"Code evidence:\n```text\ninternal/server.go:42\n```\n"
	payload := `{"verdict":"FAIL","outcome":"infra_failure","failures_markdown":` + strconv.Quote(report) + `}`

	got, _ := classifyTestOutcome("completed", payload, "", &Execution{Variables: map[string]string{}}, testVerdictSourceStep)
	if got != testOutcomeInfraFailure {
		t.Fatalf("outcome = %q, want %q", got, testOutcomeInfraFailure)
	}
	normalized := normalizeStructuredFailuresMarkdown(report, testOutcomeInfraFailure)
	if !strings.Contains(normalized, "Classification: "+testOutcomeInfraFailure) {
		t.Fatalf("normalized report missing inserted classification:\n%s", normalized)
	}
}

func TestClassifyTestOutcome_AcceptsVerbatimOutputEvidence(t *testing.T) {
	t.Parallel()

	report := "## Test Failures\n\n" +
		"### product_bug: parser rejects valid evidence\n\n" +
		"**Command run:**\n\n```bash\ngo test ./internal/workflow\n```\n\n" +
		"**Verbatim output:**\n\n```text\n--- FAIL: TestEvidence\n```\n\n" +
		"**Expected behaviour:** The task says grounded product defects route back to implementation.\n\n" +
		"**Actual behaviour:** The task escalated to human-required.\n\n" +
		"**Code evidence:**\n\n```text\ninternal/workflow/engine_steps_testroute.go:659\n```\n"
	payload := `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(report) + `}`

	got, fingerprint := classifyTestOutcome("completed", payload, "", &Execution{Variables: map[string]string{}}, testVerdictSourceStep)
	if got != testOutcomeProductBug {
		t.Fatalf("outcome = %q, want %q", got, testOutcomeProductBug)
	}
	if fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
}

func TestHasGroundedFailureEvidence_AcceptsAnnotatedLabels(t *testing.T) {
	t.Parallel()

	// Reproduces task b9d9181f: a fully grounded FAIL report whose headers were
	// annotated with a parenthetical qualifier (e.g. "Expected (task's own
	// words):"), which defeated the literal "expected:" substring check and
	// forced an unnecessary human escalation.
	report := "## Test Failures\n\n" +
		"**Command run:** `curl -fsS http://127.0.0.1:8080/rpc`\n\n" +
		"**Actual (observed):**\n\n```text\nHTTP/1.1 404 Not Found\n" +
		`{"error":"unknown method: TaskService.BlessTampering"}` + "\n```\n\n" +
		"**Expected (task's own words):** the GUI blesses tamper-flagged tasks in one click.\n\n" +
		"**Code evidence:** internal/sybra/services.go missing BlessTampering from allowlist.\n"

	if !hasGroundedFailureEvidence(report) {
		t.Fatal("annotated-label FAIL report rejected as ungrounded; want grounded")
	}
}

func TestHasGroundedFailureEvidence_RejectsUngroundedReport(t *testing.T) {
	t.Parallel()

	if hasGroundedFailureEvidence("The feature seems broken and should be fixed.") {
		t.Fatal("prose-only report accepted as grounded; want rejected")
	}
}

func TestHasGroundedFailureEvidence_RejectsBareHeadersWithNoContent(t *testing.T) {
	t.Parallel()

	// A report that stacks empty headers with nothing after the colon must not
	// satisfy the gate — only the "code evidence" header here carries real
	// content, so command/observed/expected evidence is missing despite the
	// headers being present.
	report := "**Command:**\n\n**Actual:**\n\n**Expected:**\n\n**Code evidence:** internal/x.go:1\n"

	if hasGroundedFailureEvidence(report) {
		t.Fatal("bare-header report with no evidence accepted as grounded; want rejected")
	}
}

func TestTestFailureSectionIgnoresStaleLookingHeading(t *testing.T) {
	t.Parallel()

	if got := testFailSectionOf("## Test Failures are stale\n\nnot current"); got != "" {
		t.Fatalf("stale-looking heading produced section %q, want empty", got)
	}
}

func TestHasReportLinePrefixNormalizesPrefixes(t *testing.T) {
	t.Parallel()

	report := "## Test Failures\n\nOutput:\n```text\nboom\n```\n"
	if !hasReportLinePrefix(report, " Output: ") {
		t.Fatal("expected normalized prefix to match Output line")
	}
}

func TestStructuredTestOutputEvidenceListsUnmarshalNull(t *testing.T) {
	t.Parallel()

	out := structuredTestOutput{
		ManualProbes:    manualProbeEvidenceList{{Command: "stale manual probe"}},
		AutomatedChecks: automatedCheckEvidenceList{{Command: "stale automated check"}},
	}
	if err := json.Unmarshal([]byte(`{"manual_probes":null,"automated_checks":null}`), &out); err != nil {
		t.Fatalf("unmarshal structured output: %v", err)
	}
	if len(out.ManualProbes) != 0 {
		t.Fatalf("manual probes len = %d, want 0", len(out.ManualProbes))
	}
	if len(out.AutomatedChecks) != 0 {
		t.Fatalf("automated checks len = %d, want 0", len(out.AutomatedChecks))
	}
}

func TestApplyTestVerdictCompletion_ClassifiesOutcomes(t *testing.T) {
	t.Parallel()

	initialBody := "## Problem\nExercise the testing gate."
	groundedReport := "\n\n## Test Failures\n\n" +
		"Requirement tested: the task says the status endpoint should return HTTP 200.\n\n" +
		"Command run:\n```sh\ncurl -i http://localhost/status\n```\n\n" +
		"Actual output:\n```text\nHTTP/1.1 500 Internal Server Error\n```\n\n" +
		"Expected: HTTP 200.\n\n" +
		"Code evidence:\n```text\ninternal/server.go:42: return http.StatusInternalServerError\n```\n"
	cases := []struct {
		name       string
		status     string
		output     string
		bodySuffix string
		want       string
		wantStatus string
		wantTaint  string
	}{
		{
			name:       "product_bug",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: groundedReport,
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:       "ambiguous_requirement",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: strings.ReplaceAll(groundedReport, "Requirement tested:", "Classification: ambiguous_requirement: task says two incompatible things\n\nRequirement tested:"),
			want:       testOutcomeAmbiguousRequirement,
			wantStatus: "completed",
		},
		{
			name:       "actual_output_mentions_ambiguous_but_product_bug",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: strings.ReplaceAll(groundedReport, "HTTP/1.1 500 Internal Server Error", `{"error":"ambiguous requirement"}`),
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:       "explicit_infra_failure_with_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: strings.ReplaceAll(groundedReport, "Requirement tested:", "Classification: infra_failure, Docker daemon unavailable\n\nRequirement tested:"),
			want:       testOutcomeInfraFailure,
			wantStatus: "failed",
		},
		{
			name:       "structured_json_outcome_with_heading",
			status:     "completed",
			output:     `{"verdict":"FAIL","outcome":"infra_failure","failures_markdown":` + strconv.Quote(strings.TrimSpace(groundedReport)) + `}`,
			bodySuffix: "",
			want:       testOutcomeInfraFailure,
			wantStatus: "failed",
		},
		{
			name:   "structured_failure_with_flexible_evidence_objects",
			status: "completed",
			output: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(strings.TrimSpace(groundedReport)) + `,` +
				`"surface_kind":"web/server","readiness_probe":{"command":"curl /status","output":"HTTP 500"},` +
				`"manual_probes":[{"command":"curl /status","status":"HTTP 500"}],"automated_checks":[{"command":"go test ./internal/workflow","output":"PASS"}]}`,
			bodySuffix: "",
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:   "structured_json_product_bug_with_scalar_evidence",
			status: "completed",
			output: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(
				"## Test Failures\n\n"+
					"Command run:\n```sh\ncurl -i http://localhost/status\n```\n\n"+
					"Verbatim output:\n```text\nHTTP/1.1 500 Internal Server Error\n```\n\n"+
					"Expected behavior: the task says the status endpoint should return HTTP 200.\n\n"+
					"Actual behavior: the endpoint returned HTTP 500.\n\n"+
					"Current source quote:\n```text\ninternal/server.go:42: return http.StatusInternalServerError\n```\n",
			) + `,"surface_kind":"web","app_started":true,"start_command":"go run ./cmd/sybra-server",` +
				`"readiness_probe":"curl /health -> ok","manual_probes":"browser observed the status failure",` +
				`"automated_checks":"go test ./internal/server -> fail","unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:   "structured_json_product_bug_with_task_expectation_and_code_line_evidence",
			status: "completed",
			output: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(
				"## Test Failures\n\n"+
					"### Wilson confidence interval lower bounds can be negative for 0/N rates\n\n"+
					"Classification: product_bug\n\n"+
					"Task expectation: the task requires Wilson confidence intervals for rates. A rate confidence interval is expected to stay within [0, 1].\n\n"+
					"Command run:\n```bash\ncurl -fsS /api/App/GetEvaluationReport | jq '.byVariant[0].ciFirstPassEstimate'\n```\n\n"+
					"Verbatim output:\n```json\n{\"wilsonLower\":-3.185582654932028E-17,\"wilsonUpper\":0.7934506856227624}\n```\n\n"+
					"Current code line evidence:\n```text\n616\t\test.WilsonLower = finiteOrZero((center - margin) / denom)\n```\n",
			) + `}`,
			bodySuffix: "",
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:   "structured_json_ignores_fenced_heading",
			status: "completed",
			output: `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(
				"Requirement tested: the task says Markdown headings render escaped.\n\n"+
					"Command run:\n```sh\ncurl /preview\n```\n\n"+
					"Observed output:\n```md\n## Test Failures\n```\n\n"+
					"Expected: escaped text.\n\n"+
					"Code evidence: internal/preview.go:42",
			) + `}`,
			bodySuffix: "",
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:       "expected_output_counts_as_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: strings.ReplaceAll(groundedReport, "Expected:", "Expected output:"),
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:       "observed_output_counts_as_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: strings.ReplaceAll(groundedReport, "Actual output:", "Observed output:"),
			want:       testOutcomeProductBug,
			wantStatus: "completed",
		},
		{
			name:       "missing_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: "\n\n## Test Failures\n\nIt broke.\n",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "explicit_product_bug_still_requires_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: "\n\n## Test Failures\n\nClassification: product_bug\n\nCommand run: curl /status\nActual output: HTTP 500\nExpected: HTTP 200 with one line JSON response\n",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "fail_with_pass_outcome_is_missing_evidence",
			status:     "completed",
			output:     `{"verdict":"FAIL","outcome":"pass","failures_markdown":` + strconv.Quote(strings.TrimSpace(groundedReport)) + `}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "expected_output_does_not_count_as_observed_output",
			status:     "completed",
			output:     `{"verdict":"FAIL"}`,
			bodySuffix: "\n\n## Test Failures\n\nCommand run: curl /status\nExpected output: HTTP 200\nCode evidence: internal/server.go:42: return http.StatusInternalServerError\n",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "infra_failure",
			status:     "completed",
			output:     `{}`,
			bodySuffix: "",
			want:       testOutcomeInfraFailure,
			wantStatus: "failed",
		},
		{
			// Literal incident shape: a codex test-runner that exited 0 with empty
			// stdout (e.g. crashed on "Reading additional input from stdin..."). The
			// empty string skips parseStructuredTestOutput (unlike "{}"), so this
			// guards the raw-empty branch that originally produced no outcome var.
			name:       "empty_stdout_completed_is_infra",
			status:     "completed",
			output:     "",
			bodySuffix: "",
			want:       testOutcomeInfraFailure,
			wantStatus: "failed",
		},
		{
			name:       "failed_process_with_pass_verdict_is_infra",
			status:     "failed",
			output:     `{"verdict":"PASS"}`,
			bodySuffix: "",
			want:       testOutcomeInfraFailure,
			wantStatus: "failed",
		},
		{
			name:       "pass_with_manual_evidence",
			status:     "completed",
			output:     structuredPassOutput(),
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_fenced_json_manual_evidence",
			status:     "completed",
			output:     fencedStructuredPassOutput(),
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_string_array_evidence",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"server","app_started":true,` +
				`"start_command":"SYBRA_HOME=$(mktemp -d) go run ./cmd/sybra-server",` +
				`"readiness_probe":"curl -fsS http://127.0.0.1:55990/health -> {\"status\":\"ok\"}",` +
				`"manual_probes":["POST /api/TaskService/ListTasks -> []","sybra-cli --json create --title smoke -> created task"],` +
				`"automated_checks":["go test ./... -> pass","go build ./cmd/sybra-server -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			// Regression: a model emitted app_started as the quoted string
			// "true" instead of a bool. A strict bool unmarshal failed the whole
			// verdict object, losing a valid PASS and misclassifying it as
			// infra_failure. flexBool must accept the string shape.
			name:   "pass_with_string_app_started",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"server","app_started":"true",` +
				`"start_command":"SYBRA_HOME=$(mktemp -d) go run ./cmd/sybra-server",` +
				`"readiness_probe":{"command":"curl -fsS http://127.0.0.1:57942/health","actual":"{\"status\":\"ok\"}"},` +
				`"manual_probes":[{"command":"curl -fsS http://127.0.0.1:57942/health","actual":"{\"status\":\"ok\"}"}],` +
				`"automated_checks":[{"command":"go test ./internal/attribution/...","actual":"pass"}],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_http_method_string_probe",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"server","app_started":true,` +
				`"start_command":"go run ./cmd/sybra-server",` +
				`"readiness_probe":"curl -fsS http://127.0.0.1:55990/health -> ok",` +
				`"manual_probes":["DELETE /api/tasks/123 -> 204"],` +
				`"automated_checks":["go test ./... -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_flexible_object_evidence",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"SvelteKit web UI with isolated mock API","app_started":true,` +
				`"start_command":"npm run dev -- --host 127.0.0.1",` +
				`"readiness_probe":{"command":"curl http://127.0.0.1:5173","output":"HTTP 200"},` +
				`"manual_probes":[{"command":"curl -s http://127.0.0.1:5173/tasks","output":"task list loaded"},{"command":"POST /api/tasks","observed":"created task visible in list"}],` +
				`"automated_checks":[{"command":"npm test -- --run workflow","output":"12 tests passed"}],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_scalar_string_evidence",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,` +
				`"start_command":"npm run dev -- --host 127.0.0.1",` +
				`"readiness_probe":"opened http://127.0.0.1:5173 and app shell rendered",` +
				`"manual_probes":"Browser UI: opened the task board, clicked the accept action, and observed the task card show accepted.",` +
				`"automated_checks":"frontend type-check passed; lint passed; unit tests passed; acceptance invariant rg found no raw rejection path","unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_status_alias_and_surface_alias",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web/server","app_started":true,` +
				`"start_command":"go run ./cmd/sybra-server",` +
				`"readiness_probe":{"command":"curl http://127.0.0.1:8080/health","status":"HTTP 200"},` +
				`"manual_probes":[{"command":"GET /api/tasks","status":"returned task list"}],` +
				`"automated_checks":[{"command":"go test ./internal/workflow","status":"PASS"}],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_numeric_readiness_status",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,` +
				`"start_command":"npm run dev",` +
				`"readiness_probe":{"command":"curl http://127.0.0.1:5173/health","status":200},` +
				`"manual_probes":[{"command":"GET /api/tasks","status":200}],` +
				`"automated_checks":[{"command":"npm test","status":true}],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_rejects_vague_browser_scalar_probe",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,` +
				`"start_command":"npm run dev -- --host 127.0.0.1",` +
				`"readiness_probe":"opened http://127.0.0.1:5173 and app shell rendered",` +
				`"manual_probes":"browser saw nothing",` +
				`"automated_checks":"npm run check -> pass","unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:   "pass_with_browser_ui_string_probe",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,` +
				`"start_command":"npm run dev",` +
				`"readiness_probe":"browser loaded the UI and displayed the board",` +
				`"manual_probes":["Using browser devtools, opened the workflow page, clicked retry, and observed the retry banner become visible."],` +
				`"automated_checks":["npm run check -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_ui_string_probe",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"UI","app_started":true,` +
				`"start_command":"npm run dev",` +
				`"readiness_probe":"browser opened app -> loaded",` +
				`"manual_probes":["clicked New Task -> form became visible"],` +
				`"automated_checks":["npm test -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:   "pass_with_kubernetes_surface_word",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"internal Kubernetes informer event handler","app_started":true,` +
				`"start_command":"kubectl apply -f testdata/informer.yaml",` +
				`"readiness_probe":"kubectl get pods confirmed the controller pod was running",` +
				`"manual_probes":[{"command":"kubectl describe pod controller","expected":"event handler active","actual":"event handler active"}],` +
				`"automated_checks":[{"command":"go test ./internal/workflow","actual":"ok"}],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_docs_surface_word_before_kubernetes_uses_docs_exemption",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"docs for Kubernetes","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"npm run check","actual":"ok"}],"unable_to_run_reason":"documentation-only update"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_weak_raw_ui_status_rejected",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,"start_command":"npm run dev","readiness_probe":"curl /health -> ok","manual_probes":["UI status: not tested"],"automated_checks":["npm test -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_mixed_library_web_surface_requires_manual_evidence",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library web UI","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./...","actual":"ok"}],"unable_to_run_reason":"pure library check"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:   "pass_with_library_exemption_rejects_negated_scalar_check",
			status: "completed",
			output: `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,` +
				`"start_command":"","readiness_probe":"","manual_probes":[],` +
				`"automated_checks":"unit tests were not run; pass assumed","unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:   "plain_text_pass_with_manual_evidence",
			status: "completed",
			output: "surface_kind: server\n" +
				"app_started: true\n" +
				"start_command: SYBRA_PORT=12345 go run ./cmd/sybra-server\n" +
				"readiness_probe: curl -fsS http://127.0.0.1:12345/health\n" +
				"manual_probes:\n" +
				"command: curl -fsS http://127.0.0.1:12345/health\n" +
				"expected: status ok\n" +
				"actual: {\"status\":\"ok\"}\n" +
				"automated_checks: go test ./internal/workflow => ok\n" +
				"unable_to_run_reason:\n" +
				"TEST_VERDICT: PASS",
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_without_manual_evidence",
			status:     "completed",
			output:     `{"verdict":"PASS"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_requires_regression_check",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./internal/foo","actual":"ok"}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_accepts_readiness_probe_evidence",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"go test ./internal/foo -> ok","manual_probes":[],"automated_checks":[],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_rejects_fabricated_readiness_probe",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"curl would have returned 200 for the health endpoint if the server were running, but this is a pure library change","manual_probes":[],"automated_checks":[],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_notest_exemption_still_requires_regression_check",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"none","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./internal/foo","actual":"ok"}],"unable_to_run_reason":"task tagged notest; no product surface changed"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_docs_exemption_still_requires_regression_check",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"docs","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"npm run check","actual":"ok"}],"unable_to_run_reason":"docs-only task"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_accepts_numeric_exit_success",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./internal/foo","status":0}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_accepts_numeric_http_success",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"curl /health","status":201}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_accepts_zero_failures_summary",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"pytest","output":"0 failed, 12 passed"}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			name:       "pass_with_library_exemption_rejects_static_only_check",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"rg -n rawThing internal","actual":"no matches"}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_failed_check_status",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./internal/foo","status":false}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_mixed_failed_check_output",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"npm test","output":"not ok 1","status":false}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_mixed_failed_passed_summary",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"pytest","output":"1 failed, 2 passed"}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_numeric_failure_status",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"curl /health","output":"returned HTTP 500","status":500}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_http_failure_output",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"curl /health","output":"returned HTTP 500"}],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_library_exemption_rejects_raw_failed_check",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"library","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":["go test ./internal/foo -> exit status 1"],"unable_to_run_reason":"pure internal-library refactor"}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:   "plain_text_pass_with_exemption_rejects_failed_check",
			status: "completed",
			output: "surface_kind: library\n" +
				"app_started: false\n" +
				"start_command:\n" +
				"readiness_probe:\n" +
				"manual_probes: []\n" +
				"automated_checks: go test ./internal/workflow -> exit status 1\n" +
				"unable_to_run_reason: pure internal-library refactor\n" +
				"TEST_VERDICT: PASS",
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_weak_browser_raw_probe_rejected",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"web","app_started":true,"start_command":"npm run dev","readiness_probe":"curl /health -> ok","manual_probes":["browser visible"],"automated_checks":["npm test -> pass"],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_non_pass_outcome_rejected",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"product_bug","failures_markdown":"","surface_kind":"server","app_started":true,"start_command":"go run ./cmd/sybra-server","readiness_probe":"curl /health","manual_probes":[{"command":"curl /health","expected":"200","actual":"200"}],"automated_checks":[],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			name:       "pass_with_failures_markdown_rejected",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"## Test Failures\n\nCommand run: curl /health","surface_kind":"server","app_started":true,"start_command":"go run ./cmd/sybra-server","readiness_probe":"curl /health","manual_probes":[{"command":"curl /health","expected":"200","actual":"200"}],"automated_checks":[],"unable_to_run_reason":""}`,
			bodySuffix: "",
			want:       testOutcomeMissingEvidence,
			wantStatus: "failed",
			wantTaint:  testProtocolMissingEvidence,
		},
		{
			// Regression: an internal Go component with no runnable product
			// surface, described in prose, must resolve to the library exemption
			// rather than being read as "surface_kind omitted".
			name:       "pass_with_internal_component_prose_uses_library_exemption",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"internal Go component/package: heartbeat component","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./pkg/intercp/catalog/...","actual":"ok"}],"unable_to_run_reason":"internal component; no standalone app surface"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			// Regression: a surface described as explicitly HAVING no surface
			// ("no HTTP/CLI/UI surface") must not be misread as owning that
			// surface, and a decorated app_started ("true (...)") counts as true.
			name:       "pass_with_negated_surface_prose_uses_library_exemption",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"internal background component (no HTTP/CLI/UI surface); heartbeat goroutine inside kuma-cp","app_started":"true (component.Start() run as a real goroutine)","start_command":"","readiness_probe":"go test ./pkg/intercp/catalog/... -> ok","manual_probes":[],"automated_checks":[],"unable_to_run_reason":"internal background goroutine; no product surface"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
		{
			// Regression: negated surface lists can be written as prose
			// ("no http or cli needed"), not only as slash-delimited clauses.
			name:       "pass_with_or_joined_negated_surface_uses_library_exemption",
			status:     "completed",
			output:     `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"internal library change, no http or cli needed for this pure internal change","app_started":false,"start_command":"","readiness_probe":"","manual_probes":[],"automated_checks":[{"command":"go test ./pkg/foo/...","actual":"ok"}],"unable_to_run_reason":"pure internal-library refactor, no CLI or HTTP surface"}`,
			bodySuffix: "",
			want:       testOutcomePass,
			wantStatus: "completed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := initialBody + tc.bodySuffix
			wf := &Execution{Variables: map[string]string{}}
			prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
			out := StepOutput{StepID: testVerdictSourceStep, Status: tc.status, Output: tc.output}
			taskInfo := TaskInfo{}
			if strings.Contains(tc.name, "_notest_") {
				taskInfo.Tags = []string{"notest"}
			}
			if strings.Contains(tc.name, "_docs_") {
				taskInfo.Tags = []string{"docs"}
			}
			violation, outcome, fingerprint := applyTestVerdictCompletion(wf, &out, body, taskInfo)
			if outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", outcome, tc.want)
			}
			if out.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", out.Status, tc.wantStatus)
			}
			if got := wf.Variables["step."+testVerdictSourceStep+"."+testVerdictOutcomeKey]; got != tc.want {
				t.Fatalf("workflow outcome = %q, want %q", got, tc.want)
			}
			if tc.wantTaint != "" && violation != tc.wantTaint {
				t.Fatalf("violation = %q, want %q", violation, tc.wantTaint)
			}
			if (tc.want == testOutcomeProductBug || tc.want == testOutcomeAmbiguousRequirement) && fingerprint == "" {
				t.Fatal("fingerprint is empty for evidenced failure")
			}
		})
	}
}

func TestHasRawReadinessProbeEvidenceRejectsHypotheticalText(t *testing.T) {
	t.Parallel()

	raw := "curl would have returned 200 for the health endpoint if the server were running, but this is a pure library change"
	if hasRawReadinessProbeEvidence(raw) {
		t.Fatalf("hypothetical readiness probe evidence was accepted: %q", raw)
	}
}

func TestHasRawReadinessProbeEvidenceAcceptsReturnedStatusText(t *testing.T) {
	t.Parallel()

	raw := "curl /health returned 200"
	if !hasRawReadinessProbeEvidence(raw) {
		t.Fatalf("readiness probe evidence was rejected: %q", raw)
	}
}

func TestHasRawReadinessProbeEvidenceAcceptsExpectedWithActualResult(t *testing.T) {
	t.Parallel()

	raw := "curl /health expected to return 200, it returned 200"
	if !hasRawReadinessProbeEvidence(raw) {
		t.Fatalf("readiness probe evidence with actual result was rejected: %q", raw)
	}
}

func structuredPassOutput() string {
	return `{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"server","app_started":true,"start_command":"SYBRA_PORT=0 go run ./cmd/sybra-server","readiness_probe":"curl -fsS http://127.0.0.1:12345/health","manual_probes":[{"command":"curl -fsS http://127.0.0.1:12345/health","expected":"HTTP 200 ok","actual":"ok"}],"automated_checks":[{"command":"go test ./internal/workflow","actual":"ok"}],"unable_to_run_reason":""}`
}

func fencedStructuredPassOutput() string {
	return "All probes passed.\n\n```json\n" +
		`{"verdict":"PASS","outcome":"pass","failures_markdown":"","surface_kind":"server","app_started":true,"start_command":"SYBRA_HOME=$TMP/home SYBRA_PORT=$PORT go run ./cmd/sybra-server","readiness_probe":"curl -fsS http://127.0.0.1:$PORT/health -> {\"status\":\"ok\"}","manual_probes":[{"command":"POST /api/App/GetEvaluationReport []","observed":"byAgentModelContribution rows present"},{"command":"attribution mode check","observed":"byAgentModel rows have attributionMode=latest_author"}],"automated_checks":[{"command":"go test ./internal/evaluation/...","status":"PASS"}],"unable_to_run_reason":""}` +
		"\n```"
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

func productBugRun(startedAt time.Time, fingerprint string) AgentRunInfo {
	return AgentRunInfo{
		Role:                   testRunnerRole,
		StartedAt:              startedAt,
		TestOutcome:            testOutcomeProductBug,
		TestFailureFingerprint: fingerprint,
	}
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

func mustGetTaskInfo(t *testing.T, tasks *memTasks, taskID string) TaskInfo {
	t.Helper()
	ti, err := tasks.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return ti
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

func TestAdvanceStep_StructuredFailureMarkdownIsAppendedAtomically(t *testing.T) {
	t.Parallel()
	engine, tasks, _ := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-structured",
		Status:    "testing",
		Body:      initialBody,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-structured", Role: testRunnerRole}},
	})

	report := "Requirement tested: the task says the status endpoint should return HTTP 200.\n\n" +
		"Command run:\n```sh\ncurl -i http://localhost/status\n```\n\n" +
		"Observed output:\n```text\nHTTP/1.1 500 Internal Server Error\n```\n\n" +
		"Expected: HTTP 200.\n\n" +
		"Code evidence:\n```text\ninternal/server.go:42: return http.StatusInternalServerError\n```\n"
	payload := `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(report) + `}`

	err := engine.AdvanceStep("t-structured", StepOutput{
		StepID:  testVerdictSourceStep,
		Status:  "completed",
		Output:  payload,
		AgentID: "agent-structured",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := tasks.GetTask("t-structured")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "## Test Failures") || !strings.Contains(got.Body, "Observed output:") {
		t.Fatalf("task body missing structured failure report:\n%s", got.Body)
	}
	if got.AgentRuns[0].TestOutcome != testOutcomeProductBug {
		t.Fatalf("test outcome = %q, want %q", got.AgentRuns[0].TestOutcome, testOutcomeProductBug)
	}
	if got.AgentRuns[0].TestFailureFingerprint == "" {
		t.Fatal("test failure fingerprint is empty")
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
}

func TestAdvanceStep_StructuredFailureAppendsAfterUnrelatedBodyDelta(t *testing.T) {
	t.Parallel()
	engine, tasks, _ := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	currentBody := initialBody + "\n\n## Test Failures are stale\n\nChecked locally while tester was running.\n"
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-structured-delta",
		Status:    "testing",
		Body:      currentBody,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-structured-delta", Role: testRunnerRole}},
	})

	report := "Requirement tested: the task says the status endpoint should return HTTP 200.\n\n" +
		"Command run:\n```sh\ncurl -i http://localhost/status\n```\n\n" +
		"Observed output:\n```text\nHTTP/1.1 500 Internal Server Error\n```\n\n" +
		"Expected: HTTP 200.\n\n" +
		"Code evidence:\n```text\ninternal/server.go:42: return http.StatusInternalServerError\n```\n"
	payload := `{"verdict":"FAIL","outcome":"product_bug","failures_markdown":` + strconv.Quote(report) + `}`

	err := engine.AdvanceStep("t-structured-delta", StepOutput{
		StepID:  testVerdictSourceStep,
		Status:  "completed",
		Output:  payload,
		AgentID: "agent-structured-delta",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := tasks.GetTask("t-structured-delta")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "## Test Failures are stale") || !strings.Contains(got.Body, "## Test Failures") {
		t.Fatalf("body should preserve unrelated delta and append test report:\n%s", got.Body)
	}
	if strings.Count(got.Body, "\n\n## Test Failures\n") != 1 {
		t.Fatalf("body should append exactly one test report section:\n%s", got.Body)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
}

func TestAdvanceStep_PlainTextFailureMarkdownIsAppendedAtomically(t *testing.T) {
	t.Parallel()
	engine, tasks, _ := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
	wf := &Execution{
		WorkflowID:  "testing-task",
		CurrentStep: testVerdictSourceStep,
		State:       ExecWaiting,
		Variables:   map[string]string{},
		StartedAt:   time.Now().UTC(),
	}
	prepareTestVerdictAttemptVars(wf, testVerdictSourceStep, initialBody)
	tasks.Put(TaskInfo{
		ID:        "t-plain",
		Status:    "testing",
		Body:      initialBody,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-plain", Role: testRunnerRole}},
	})
	output := "## Test Failures\n\n" +
		"Requirement tested: the task says the status endpoint should return HTTP 200.\n\n" +
		"Command run:\n```sh\ncurl -i http://localhost/status\n```\n\n" +
		"Observed output:\n```text\nTEST_VERDICT: FAIL\n```\n\n" +
		"Expected: HTTP 200.\n\n" +
		"Code evidence:\n```text\ninternal/server.go:42: return http.StatusInternalServerError\n```\n\n" +
		"TEST_VERDICT: FAIL"

	err := engine.AdvanceStep("t-plain", StepOutput{
		StepID:  testVerdictSourceStep,
		Status:  "completed",
		Output:  output,
		AgentID: "agent-plain",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := tasks.GetTask("t-plain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "## Test Failures") || !strings.Contains(got.Body, "Observed output:") {
		t.Fatalf("task body missing plain-text failure report:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, testVerdictFail) {
		t.Fatalf("task body should preserve verdict-shaped observed output:\n%s", got.Body)
	}
	if count := strings.Count(got.Body, testVerdictFail); count != 1 {
		t.Fatalf("task body should strip only the final verdict marker, count=%d:\n%s", count, got.Body)
	}
	if got.AgentRuns[0].TestOutcome != testOutcomeProductBug {
		t.Fatalf("test outcome = %q, want %q", got.AgentRuns[0].TestOutcome, testOutcomeProductBug)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
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

func TestAdvanceStep_FailedRunnerWithPassMarkerRoutesAsInfraFailure(t *testing.T) {
	t.Parallel()
	engine, tasks, agents := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
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
		ID:        "t-failed-pass",
		Status:    "testing",
		Body:      initialBody,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-failed-pass", Role: testRunnerRole}},
	})

	err := engine.AdvanceStep("t-failed-pass", StepOutput{
		StepID:  testVerdictSourceStep,
		Status:  "failed",
		Output:  "runner crashed after printing\nTEST_VERDICT: PASS",
		AgentID: "agent-failed-pass",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("retry StartAgent calls = %d, want 0 after retry budget exhausted", got)
	}
	got, err := tasks.GetTask("t-failed-pass")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t-failed-pass"); !strings.Contains(reason, "infrastructure failed") {
		t.Errorf("status reason = %q, want infrastructure failure", reason)
	}
	if got.AgentRuns[0].TestOutcome != testOutcomeInfraFailure {
		t.Errorf("test outcome = %q, want %q", got.AgentRuns[0].TestOutcome, testOutcomeInfraFailure)
	}
}

// TestAdvanceStep_CompletedRunnerWithEmptyStdoutRoutesAsInfraFailure reproduces
// the original incident: a codex test-runner exited 0 with empty stdout (crashed
// on "Reading additional input from stdin..."), so no TEST_VERDICT marker was
// produced. Such a run must classify as an infrastructure
// failure and escalate WITHOUT consuming a product-bug implementation attempt —
// before #1176 the empty-output completion produced no outcome var and fell
// through to the attempt-cap counter. Empty stdout (unlike "{}") skips structured
// parsing, so this guards the exact branch that burned the cap.
func TestAdvanceStep_CompletedRunnerWithEmptyStdoutRoutesAsInfraFailure(t *testing.T) {
	t.Parallel()
	engine, tasks, agents := makeTestingTaskEngine(t)

	initialBody := "## Problem\nExercise the testing gate."
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
		ID:        "t-empty-stdout",
		Status:    "testing",
		Body:      initialBody,
		AgentMode: "headless",
		Workflow:  wf,
		AgentRuns: []AgentRunInfo{{AgentID: "agent-empty", Role: testRunnerRole}},
	})

	err := engine.AdvanceStep("t-empty-stdout", StepOutput{
		StepID:   testVerdictSourceStep,
		Status:   "completed",
		Output:   "",
		AgentID:  "agent-empty",
		Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := agents.CallCount(); got != 0 {
		t.Fatalf("StartAgent calls = %d, want 0 — an infra crash must not dispatch an implementation agent", got)
	}
	got, err := tasks.GetTask("t-empty-stdout")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	reason := tasks.Reason("t-empty-stdout")
	if !strings.Contains(reason, "no implementation attempt consumed") {
		t.Errorf("status reason = %q, want no implementation attempt consumed", reason)
	}
	if got.AgentRuns[0].TestOutcome != testOutcomeInfraFailure {
		t.Errorf("test outcome = %q, want %q", got.AgentRuns[0].TestOutcome, testOutcomeInfraFailure)
	}
	// An infra-classified run is skipped by the product-bug attempt counter, so it
	// can never reach the escalation/duplicate paths that re-implement or burn the cap.
	if got.AgentRuns[0].TestFailureFingerprint != "" {
		t.Errorf("fingerprint = %q, want empty for infra failure", got.AgentRuns[0].TestFailureFingerprint)
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
	runs := []AgentRunInfo{productBugRun(now, "fp-1")}
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
		productBugRun(now, "fp-1"),
		productBugRun(now.Add(time.Minute), "fp-2"),
		productBugRun(now.Add(2*time.Minute), "fp-3"),
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
		productBugRun(now.Add(time.Minute), "fp-1"),
		productBugRun(now.Add(2*time.Minute), "fp-2"),
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

func TestRouteTestResult_LegacyEmptyOutcomeRunsDoNotCountTowardCap(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	runs := []AgentRunInfo{
		{AgentID: "legacy-1", Role: testRunnerRole, StartedAt: now},
		{AgentID: "legacy-2", Role: testRunnerRole, StartedAt: now.Add(time.Minute)},
		{AgentID: "legacy-3", Role: testRunnerRole, StartedAt: now.Add(2 * time.Minute)},
		productBugRun(now.Add(3*time.Minute), "current-grounded"),
	}
	out, err := runRouteTestResult(e, tasks, "t3-legacy", "FAIL", now, runs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "reimplement" {
		t.Errorf("output = %q, want reimplement", out.Output)
	}
	ti, _ := tasks.GetTask("t3-legacy")
	if ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", ti.Status)
	}
}

func TestRouteTestResult_InfraFailureDoesNotCountTowardCap(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	runs := []AgentRunInfo{
		{AgentID: "valid-1", Role: testRunnerRole, StartedAt: now, TestOutcome: testOutcomeProductBug, TestFailureFingerprint: "fp-1"},
		{AgentID: "infra", Role: testRunnerRole, StartedAt: now.Add(time.Minute), TestOutcome: testOutcomeInfraFailure},
	}
	tasks.Put(TaskInfo{
		ID:        "t-infra",
		Status:    "testing",
		AgentRuns: runs,
	})
	wf := &Execution{
		WorkflowID: "testing-task",
		StartedAt:  now,
		Variables: map[string]string{
			"step." + testVerdictSourceStep + "." + testVerdictOutcomeKey: testOutcomeInfraFailure,
		},
	}
	out, err := e.execRouteTestResult("t-infra", &Step{ID: "route_test"}, wf, mustGetTaskInfo(t, tasks, "t-infra"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "infra failure" {
		t.Errorf("output = %q, want infra failure", out.Output)
	}
	ti, _ := tasks.GetTask("t-infra")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t-infra"); !strings.Contains(reason, "no implementation attempt consumed") {
		t.Errorf("reason = %q, want no implementation attempt consumed", reason)
	}
}

func TestRouteTestResult_DuplicateFailureEscalatesWithoutAnotherRetry(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	fp := "same-repro"
	runs := []AgentRunInfo{
		{AgentID: "first", Role: testRunnerRole, StartedAt: now, TestOutcome: testOutcomeProductBug, TestFailureFingerprint: fp},
		{AgentID: "impl", Role: "implementation", StartedAt: now.Add(time.Minute)},
		{AgentID: "second", Role: testRunnerRole, StartedAt: now.Add(2 * time.Minute), TestOutcome: testOutcomeProductBug, TestFailureFingerprint: fp},
	}
	tasks.Put(TaskInfo{
		ID:        "t-dup",
		Status:    "testing",
		AgentRuns: runs,
	})
	wf := &Execution{
		WorkflowID: "testing-task",
		StartedAt:  now,
		Variables: map[string]string{
			"step." + testVerdictSourceStep + ".verdict":                      "FAIL",
			"step." + testVerdictSourceStep + "." + testVerdictOutcomeKey:     testOutcomeProductBug,
			"step." + testVerdictSourceStep + "." + testFailureFingerprintKey: fp,
		},
	}
	out, err := e.execRouteTestResult("t-dup", &Step{ID: "route_test"}, wf, mustGetTaskInfo(t, tasks, "t-dup"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "duplicate failure" {
		t.Errorf("output = %q, want duplicate failure", out.Output)
	}
	ti, _ := tasks.GetTask("t-dup")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t-dup"); !strings.Contains(reason, "same grounded test failure") {
		t.Errorf("reason = %q, want duplicate failure", reason)
	}
}

func TestRouteTestResult_DuplicateFailureWithoutInterveningFixDoesNotEscalate(t *testing.T) {
	t.Parallel()
	e, tasks := makeTestEngine(t)
	now := time.Now().UTC()
	fp := "same-repro"
	runs := []AgentRunInfo{
		{AgentID: "first", Role: testRunnerRole, StartedAt: now, TestOutcome: testOutcomeProductBug, TestFailureFingerprint: fp},
		{AgentID: "retry", Role: testRunnerRole, StartedAt: now.Add(time.Minute), TestOutcome: testOutcomeProductBug, TestFailureFingerprint: fp},
	}
	tasks.Put(TaskInfo{
		ID:        "t-dup-no-fix",
		Status:    "testing",
		AgentRuns: runs,
	})
	wf := &Execution{
		WorkflowID: "testing-task",
		StartedAt:  now,
		Variables: map[string]string{
			"step." + testVerdictSourceStep + ".verdict":                      "FAIL",
			"step." + testVerdictSourceStep + "." + testVerdictOutcomeKey:     testOutcomeProductBug,
			"step." + testVerdictSourceStep + "." + testFailureFingerprintKey: fp,
		},
	}
	out, err := e.execRouteTestResult("t-dup-no-fix", &Step{ID: "route_test"}, wf, mustGetTaskInfo(t, tasks, "t-dup-no-fix"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Output != "reimplement" {
		t.Errorf("output = %q, want reimplement", out.Output)
	}
	ti, _ := tasks.GetTask("t-dup-no-fix")
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
		productBugRun(priorCycleEnd.Add(-2*time.Minute), "old-1"),
		productBugRun(priorCycleEnd.Add(-time.Minute), "old-2"),
		productBugRun(priorCycleEnd, "old-3"),
	}
	// 1 new run in the current cycle — must NOT count prior runs toward the cap.
	priorRuns = append(priorRuns, productBugRun(newCycleStart.Add(time.Minute), "new-1"))
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
		productBugRun(priorCycleEnd.Add(-2*time.Minute), "old-1"),
		productBugRun(priorCycleEnd.Add(-time.Minute), "old-2"),
		productBugRun(priorCycleEnd, "old-3"),
	}
	// 3 new-cycle runs — cap=3 → escalate.
	newRuns := []AgentRunInfo{
		productBugRun(newCycleStart.Add(time.Minute), "new-1"),
		productBugRun(newCycleStart.Add(2*time.Minute), "new-2"),
		productBugRun(newCycleStart.Add(3*time.Minute), "new-3"),
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
