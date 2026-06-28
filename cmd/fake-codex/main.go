// fake-codex is a test double for the codex CLI. It reads FAKE_CODEX_SCENARIO
// to decide behavior, logs received args, and outputs canned JSONL events.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// cleanEnvPath validates a file path from an env var.
// Only paths under the system temp directory are accepted to prevent traversal.
// Returns empty string if the path is empty, unresolvable, or outside tmp.
func cleanEnvPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return ""
	}
	tmpRoot := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	if !strings.HasPrefix(abs+string(filepath.Separator), tmpRoot) {
		return ""
	}
	return abs
}

func main() {
	if len(os.Args) > 3 && os.Args[1] == "plugin" && os.Args[2] == "list" && os.Args[3] == "--json" {
		fmt.Println(`{"installed":[],"available":[]}`)
		return
	}

	if logFile := cleanEnvPath(os.Getenv("FAKE_CODEX_ARGS_LOG")); logFile != "" {
		_ = os.WriteFile(logFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}

	if len(os.Args) > 1 && os.Args[1] == "exec" {
		runExec()
		return
	}
	runInteractive()
}

func runExec() {
	scenario := popScenario()
	taskID := extractTaskID(os.Args)

	emit(map[string]any{"type": "thread.started", "thread_id": "fake-thread-1"})
	emit(map[string]any{"type": "turn.started"})

	switch scenario {
	case "success":
		emitAgentMessage("Working on it...")
		emitTurnCompleted(100, 20)
	case "write_sidecar_success":
		runCodexWriteSidecarSuccess(taskID)
	case "fail_exit":
		emitError("command failed")
		os.Exit(1)
	case "no_result":
		emitAgentMessage("Working on it...")
		os.Exit(0)
	case "triage":
		codexTriage(taskID, "--status", "todo", "--tags", "small")
	case "triage_to_planning":
		codexTriage(taskID, "--status", "planning", "--tags", "large")
	case "triage_to_planning_nocritic":
		codexTriage(taskID, "--status", "planning", "--tags", "large,nocritic")
	case "triage_to_planning_noplan":
		codexTriage(taskID, "--status", "planning", "--tags", "large,noplan")
	case "plan_critic_success":
		runCodexPlanCriticSuccess(taskID)
	case "plan_critic_no_save":
		runCodexPlanCriticNoSave()
	case "code_review_success":
		runCodexCodeReviewSuccess(taskID)
	case "triage_to_done":
		codexTriage(taskID, "--status", "done")
	case "triage_to_in_review":
		codexTriage(taskID, "--status", "in-review")
	case "triage_to_human_required":
		codexTriage(taskID, "--status", "human-required")
	case "overloaded_error":
		emitError("Service overloaded (529)")
		os.Exit(1)
	case "overloaded_error_structured":
		emit(map[string]any{
			"type":    "error",
			"message": "Service overloaded",
			"code":    529,
		})
		os.Exit(1)
	case "test_verdict_pass":
		runCodexTestVerdictPass()
	case "test_verdict_fail":
		runCodexTestVerdictFail()
	case "implement", "interactive_implement":
		emitAgentMessage("Implementing...")
		emitTurnCompleted(100, 20)
	case "evaluate":
		emitAgentMessage("Evaluating...")
		runCLI(taskID, "update", taskID, "--status", "in-review")
		emitTurnCompleted(100, 20)
	case "pr_created":
		emitAgentMessage("Implementation done. Created PR https://github.com/test-org/test-repo/pull/42")
		emitTurnCompleted(100, 20)
	case "auth_error":
		emitError("Authentication failed")
		os.Exit(1)
	case "malformed_pr_output":
		runCodexMalformedPR()
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}
}

func runCodexTestVerdictPass() {
	emitAgentMessage(mustJSON(map[string]any{
		"verdict":           "PASS",
		"outcome":           "pass",
		"failures_markdown": "",
		"surface_kind":      "cli",
		"app_started":       true,
		"start_command":     "sybra-cli --json list",
		"readiness_probe":   "sybra-cli --json list",
		"manual_probes": []map[string]string{{
			"command":  "sybra-cli --json list",
			"expected": "returns JSON task list",
			"actual":   "[]",
		}},
		"automated_checks":     []map[string]string{},
		"unable_to_run_reason": "",
	}))
	emitTurnCompleted(100, 20)
}

func runCodexTestVerdictFail() {
	emitAgentMessage(mustJSON(map[string]any{
		"verdict":           "FAIL",
		"outcome":           "product_bug",
		"failures_markdown": testFailureReport(),
		"surface_kind":      "server",
		"app_started":       true,
		"start_command":     "go run ./cmd/test-server",
		"readiness_probe":   "curl /status",
		"manual_probes": []map[string]string{{
			"command":  "curl /status",
			"expected": "expected output",
			"actual":   "wrong output",
		}},
		"automated_checks":     []map[string]string{},
		"unable_to_run_reason": "",
	}))
	emitTurnCompleted(100, 20)
}

func runCodexMalformedPR() {
	var b strings.Builder
	for range 200 {
		b.WriteString("note: saw github.com/test-org/test-repo/pul/42 and github.com/test-org/test-repo/pulls/42\n")
	}
	emitAgentMessage(b.String())
	emitTurnCompleted(100, 20)
}

// runCodexWriteSidecarSuccess mirrors fake-claude's write_sidecar_success:
// extracts the import-sidecar path from the prompt and writes a stub so
// the engine can ingest it for the workflow step's import_sidecar.
func runCodexWriteSidecarSuccess(taskID string) {
	emitAgentMessage("Writing fake sidecar...")
	for _, path := range extractSidecarPaths(os.Args) {
		_ = os.WriteFile(path, []byte(fakeSidecarContent(path, taskID, "fake-codex")), 0o644)
	}
	emitTurnCompleted(100, 20)
}

func runCodexPlanCriticSuccess(taskID string) {
	emitAgentMessage("Critiquing plan...")
	runCLI(taskID, "update", taskID, "--plan-critique", "# Plan Critique\n\n## Verdict: REFINE\n\n- Consider edge case X.\n")
	emitTurnCompleted(100, 20)
}

func runCodexPlanCriticNoSave() {
	emitAgentMessage("Blocked by env. Did not save critique.")
	emitTurnCompleted(100, 20)
}

func runCodexCodeReviewSuccess(taskID string) {
	emitAgentMessage("Reviewing code...")
	runCLI(taskID, "update", taskID, "--code-review", "# Code Review\n\nLooks good.\n")
	emitTurnCompleted(100, 20)
}

func runInteractive() {
	// Keep the process alive long enough for e2e assertions.
	time.Sleep(5 * time.Second)
}

func emitAgentMessage(text string) {
	emit(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "item_0",
			"type": "agent_message",
			"text": text,
		},
	})
}

func emitTurnCompleted(inputTokens, outputTokens int) {
	emit(map[string]any{
		"type": "turn.completed",
		"usage": map[string]any{
			"input_tokens":  float64(inputTokens),
			"output_tokens": float64(outputTokens),
		},
	})
}

func emitError(message string) {
	emit(map[string]any{
		"type":    "error",
		"message": message,
	})
}

func emit(event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
	time.Sleep(10 * time.Millisecond)
}

var taskIDRe = regexp.MustCompile(`\b([a-f0-9]{8})\b`)

func extractTaskID(args []string) string {
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) {
			if matches := taskIDRe.FindStringSubmatch(args[i+1]); len(matches) > 1 {
				return matches[1]
			}
		}
		if i == len(args)-1 {
			if matches := taskIDRe.FindStringSubmatch(arg); len(matches) > 1 {
				return matches[1]
			}
		}
	}
	return ""
}

// sidecarPathRe matches `/tmp/sybra-...` sidecar paths in rendered prompts —
// see fake-claude/main.go for the matching pattern. Used by the
// write_sidecar_success scenario so a single fake-agent variant can serve
// any import_sidecar workflow step.
var sidecarPathRe = regexp.MustCompile(`/tmp/sybra-[A-Za-z0-9_./-]+\.(?:md|json)`)

func extractSidecarPaths(args []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for i, arg := range args {
		// codex CLI accepts the prompt as a positional argument (not -p).
		if arg == "-p" && i+1 < len(args) {
			for _, m := range sidecarPathRe.FindAllString(args[i+1], -1) {
				if _, ok := seen[m]; ok {
					continue
				}
				seen[m] = struct{}{}
				out = append(out, m)
			}
		}
		for _, m := range sidecarPathRe.FindAllString(arg, -1) {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

func fakeSidecarContent(path, taskID, producer string) string {
	base := filepath.Base(path)
	switch {
	case strings.Contains(base, "plan-research"):
		return "# Research\n\nStub generated by " + producer + " for " + taskID + ".\n"
	case strings.Contains(base, "plan-decisions"):
		return "# Decisions\n\nNo open decisions. The recommended execution contract is fully specified.\n"
	case strings.Contains(base, "plan-brief"):
		return "# Final Brief\n\nStub brief generated by " + producer + " for " + taskID + ".\n"
	case strings.Contains(base, "plan-contract"):
		return fakePlanContract(taskID)
	default:
		return "# Execution Plan\n\n## Decision\nUse the fake execution path.\n\n## Scope\n- In: fake test implementation.\n- Out: production behavior.\n\n## Files\n- `fake` - test fixture.\n\n## Steps\n1. Run the fake workflow.\n\n## Verification\n- `go test` - green.\n\n## Stop Conditions\n- Stop if fake sidecars are missing.\n"
	}
}

func fakePlanContract(taskID string) string {
	if taskID == "" {
		taskID = "00000000"
	}
	return fmt.Sprintf(`{
  "task_id": %q,
  "branch": "feat/fake-%s",
  "worktree": "/tmp/sybra-worktrees/fake-%s",
  "files": [
    {"path": "internal/sybra/e2e_workflow_test.go", "purpose": "test"}
  ],
  "steps": ["drive the fake workflow"],
  "verification": [
    {"command": "go test ./internal/sybra", "expected": "tests pass"}
  ],
  "acceptance_criteria": ["workflow reaches the expected gate"],
  "risk_tier": "low",
  "permission_tier": "repo-write",
  "rollback": "revert the fake workflow fixture changes"
}
`, taskID, taskID, taskID)
}

func popScenario() string {
	if sf := cleanEnvPath(os.Getenv("FAKE_CODEX_SCENARIO_FILE")); sf != "" {
		data, err := os.ReadFile(sf)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) > 0 && lines[0] != "" {
				scenario := strings.TrimSpace(lines[0])
				remaining := strings.Join(lines[1:], "\n")
				_ = os.WriteFile(sf, []byte(remaining), 0o644)
				return scenario
			}
		}
	}
	if s := os.Getenv("FAKE_CODEX_SCENARIO"); s != "" {
		return s
	}
	return "success"
}

// codexTriage emits a triage turn that updates the task via sybra-cli with the
// given fields (e.g. --status/--tags), mirroring the real codex triage step.
func codexTriage(taskID string, updateArgs ...string) {
	emitAgentMessage("Triaging task...")
	runCLI(taskID, "update", append([]string{taskID}, updateArgs...)...)
	emitTurnCompleted(100, 20)
}

func runCLI(taskID, subcmd string, args ...string) {
	if taskID == "" {
		return
	}
	bin, err := exec.LookPath("sybra-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sybra-cli not found: %v\n", err)
		return
	}
	cmdArgs := append([]string{"--json", subcmd}, args...)
	cmd := &exec.Cmd{
		Path:   bin,
		Args:   append([]string{bin}, cmdArgs...),
		Stdout: os.Stderr,
		Stderr: os.Stderr,
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sybra-cli failed: %v\n", err)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func testFailureReport() string {
	return "## Test Failures\n\n" +
		"Classification: product_bug\n\n" +
		"Requirement tested: the task says the happy path should render the expected output.\n\n" +
		"Command run:\n```sh\ncurl /status\n```\n\n" +
		"Actual output:\n```text\nwrong output\n```\n\n" +
		"Expected output: expected output.\n\n" +
		"Code evidence:\n```text\ninternal/fake.go:42: return \"wrong output\"\n```"
}
