// fake-claude is a test double for the claude CLI. It reads FAKE_CLAUDE_SCENARIO
// to decide behavior, logs received args, and outputs canned NDJSON.
//
// Scenario selection (in priority order):
//  1. FAKE_CLAUDE_SCENARIO_FILE: path to a file with one scenario per line.
//     Each invocation pops and uses the first line (for multi-step workflows).
//  2. FAKE_CLAUDE_SCENARIO: static scenario name for single-step tests.
//  3. Default: "success"
//
// Scenarios:
//   - success (default): system + assistant + result events
//   - fail_exit: system event then exit 1
//   - no_result: system + assistant, exit 0 (no result event)
//   - triage: runs sybra-cli to set status=todo, tags=small, emits result
//   - triage_to_planning: runs sybra-cli to set status=planning, tags=large
//   - triage_to_planning_nocritic: like triage_to_planning but adds nocritic tag
//   - triage_to_planning_noplan: like triage_to_planning but adds noplan tag
//   - triage_to_done: runs sybra-cli to set status=done
//   - triage_to_in_review: runs sybra-cli to set status=in-review
//   - triage_to_human_required: runs sybra-cli to set status=human-required
//   - implement: emits result with "PR created" text
//   - interactive_implement: emits result then blocks on stdin until EOF,
//     simulating a real conversational claude agent that stays alive between
//     turns. Exits when the parent closes stdin — e.g. the one-shot runner
//     path that closes stdin after the first result event.
//   - evaluate: runs sybra-cli to set status=in-review, emits result
//   - pr_created: emits result with a github.com/.../pull/N URL so the
//     mechanical link_pr_and_review step can extract the PR number via regex
//   - test_infra_failure: emits a grounded FAIL report classified as
//     infra_failure, simulating an un-runnable manual gate
//   - auth_error: emits auth-failure text then exits 1
//   - malformed_pr_output: emits large malformed PR-ish text (no valid URL)
//   - signal_kill: emits system+assistant then kills self with SIGTERM
//   - block_silent: emits nothing and blocks on stdin until EOF. Simulates the
//     wedged orchestrator brain — a conversational agent started with no
//     kickoff prompt that parks in StatePaused with no session id.
//   - hang: emits system+assistant then blocks indefinitely. Used to simulate
//     an agent that Sybra's StopAgent has to kill mid-run.
//   - sybra_home_sentinel: writes a marker under $SYBRA_HOME, then runs bare
//     `sybra-cli update` (no --home) on the task — proves default writes land
//     in the per-task sandbox while task CRUD still reaches the real store
//     via SYBRA_CONTROL_HOME (sybra#1577).
//
// Perf scenarios (zero token cost, drive backend load):
//   - perf_stream: emit FAKE_CLAUDE_EVENT_COUNT assistant events spaced
//     FAKE_CLAUDE_EVENT_INTERVAL_MS apart, then a result event. Defaults:
//     100 events, 10ms interval.
//   - perf_burst: emit FAKE_CLAUDE_EVENT_COUNT assistant events with zero
//     inter-event sleep (stresses the 50ms emit throttle in runner_headless).
//     Default: 500 events.
//   - perf_long: emit assistant events at FAKE_CLAUDE_EVENT_INTERVAL_MS cadence
//     for FAKE_CLAUDE_DURATION_MS total, then a result event. Used for soak
//     and memory-leak testing. Defaults: 30s duration, 200ms interval.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// scenarioFileLockSuffix is appended to FAKE_CLAUDE_SCENARIO_FILE to derive a
// per-file lock path that flock(2) can serialize across concurrent fake-claude
// processes. We don't lock the scenario file directly because we read+truncate
// it; a sibling lockfile keeps the lock target stable.
const scenarioFileLockSuffix = ".lock"

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
	// Log args for test verification.
	if logFile := cleanEnvPath(os.Getenv("FAKE_CLAUDE_ARGS_LOG")); logFile != "" {
		_ = writeArgsLog(logFile, []byte(strings.Join(os.Args[1:], "\n")))
	}

	scenario := popScenario()
	prompt := promptArg(os.Args)
	if prompt == "" && scenarioNeedsPromptContext(scenario) {
		prompt = readInitialPrompt(os.Stdin)
	}
	if !runScenario(scenario, promptTaskID(prompt)) {
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}
}

func writeArgsLog(logFile string, data []byte) error {
	dir := filepath.Dir(logFile)
	tmp, err := os.CreateTemp(dir, ".claude-args-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, logFile); err != nil {
		return err
	}
	cleanup = false
	return nil
}

var scenarioHandlers = map[string]func(string){
	"success":                     func(string) { runSuccess() },
	"high_cost":                   func(string) { runHighCost() },
	"write_sidecar_success":       runWriteSidecarSuccess,
	"revise_plan_sidecars":        runRevisePlanSidecars,
	"fail_exit":                   func(string) { emitSystem(); os.Exit(1) },
	"no_result":                   func(string) { emitSystem(); emitAssistant("Working on it..."); os.Exit(0) },
	"triage":                      func(taskID string) { runTriage(taskID, "todo", "small") },
	"triage_to_planning":          func(taskID string) { runTriage(taskID, "planning", "large") },
	"triage_to_planning_nocritic": func(taskID string) { runTriage(taskID, "planning", "large,nocritic") },
	"triage_to_planning_noplan":   func(taskID string) { runTriage(taskID, "planning", "large,noplan") },
	"plan_critic_success":         runPlanCriticSuccess,
	"plan_critic_no_save":         func(string) { runPlanCriticNoSave() },
	"code_review_success":         runCodeReviewSuccess,
	"test_pass":                   func(string) { runTestPass() },
	"test_pass_verbose":           func(string) { runTestPassVerbose() },
	"test_fail":                   func(string) { runTestFail() },
	"test_infra_failure":          func(string) { runTestInfraFailure() },
	"tool_result_large":           func(string) { runToolResultLarge() },
	"malformed_tool_call_once":    func(string) { runMalformedToolCallOnce() },
	"malformed_tool_call_repeat":  func(string) { runMalformedToolCallRepeat() },
	"triage_to_done":              func(taskID string) { runTriage(taskID, "done", "") },
	"triage_to_in_review":         func(taskID string) { runTriage(taskID, "in-review", "") },
	"triage_to_human_required":    func(taskID string) { runTriage(taskID, "human-required", "") },
	"implement": func(string) {
		emitSystem()
		emitAssistant("Implementing...")
		emitResult("Implementation done. PR created")
	},
	"interactive_implement": func(string) { runInteractiveImplement() },
	"evaluate":              runEvaluate,
	"pr_created":            func(string) { runPRCreated() },
	"signal_kill":           func(string) { runSignalKill() },
	"block_silent":          func(string) { _, _ = io.Copy(io.Discard, os.Stdin) },
	"hang":                  func(string) { runHang() },
	"success_then_hang":     func(string) { runSuccessThenHang() },
	"auth_error":            func(string) { emitSystem(); emitAssistant("Authentication failed. Please re-auth."); os.Exit(1) },
	"malformed_pr_output":   func(string) { runMalformedPROutput() },
	"perf_stream":           func(string) { runPerfStream() },
	"perf_burst":            func(string) { runPerfBurst() },
	"perf_long":             func(string) { runPerfLong() },
	"sybra_home_sentinel":   runSybraHomeSentinel,
	"best_of_n_attempt":     func(string) { runBestOfNAttempt() },
	"best_of_n_judge":       func(string) { runBestOfNJudge() },
}

func scenarioNeedsPromptContext(scenario string) bool {
	switch scenario {
	case "triage",
		"triage_to_planning",
		"triage_to_planning_nocritic",
		"triage_to_planning_noplan",
		"triage_to_done",
		"triage_to_in_review",
		"triage_to_human_required",
		"evaluate",
		"write_sidecar_success",
		"revise_plan_sidecars",
		"plan_critic_success":
		return true
	default:
		return false
	}
}

func readInitialPrompt(r io.Reader) string {
	br := bufio.NewReader(r)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	return parseUserPromptLine(line)
}

func parseUserPromptLine(line []byte) string {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
		return ""
	}
	if msg.Type != "user" {
		return ""
	}
	return strings.TrimSpace(msg.Message.Content)
}

// runBestOfNAttempt simulates one best-of-N implementation attempt: it
// derives a unique filename from its own cwd (the attempt's isolated
// worktree dir, e.g. ".../task-attempt_2") and commits it there. Using cwd
// rather than an env var means every attempt agent can share this single
// scenario name — dispatch order between concurrently-running attempts
// never matters, unlike a scenario-file sequence keyed by invocation order.
// Never pushes, matching the best-of-n attempt contract (PromoteAttempt
// moves the canonical branch locally once a winner is judged).
func runBestOfNAttempt() {
	emitSystem()
	cwd, err := os.Getwd()
	if err != nil {
		emitResult("best_of_n_attempt: getwd failed: " + err.Error())
		os.Exit(1)
	}
	name := filepath.Base(cwd)
	fname := "attempt-" + name + ".txt"
	if err := os.WriteFile(filepath.Join(cwd, fname), []byte("attempt work from "+name+"\n"), 0o644); err != nil {
		emitResult("best_of_n_attempt: write failed: " + err.Error())
		os.Exit(1)
	}
	for _, args := range [][]string{
		{"config", "user.email", "fake-claude@test.local"},
		{"config", "user.name", "Fake Claude"},
		{"add", fname},
		{"commit", "-m", "best-of-n attempt work: " + name},
	} {
		runGitIn(cwd, args...)
	}
	emitAssistant("Implemented attempt in " + name)
	emitResult("Implementation done for " + name + ". Committed, did not push.")
}

// runBestOfNJudge simulates the automated judge: it always names attempt_2
// the winner, giving e2e tests a deterministic, mechanically-parseable
// verdict without needing the fake binary to actually read/diff attempts.
func runBestOfNJudge() {
	emitSystem()
	emitAssistant("Judging attempts...")
	emitResult(`{"winner_attempt_id": "attempt_2", "scores": [{"attempt_id":"attempt_1","score":5},{"attempt_id":"attempt_2","score":9}], "rationale": "attempt_2 is more complete"}`)
}

func runGitIn(dir string, args ...string) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		emitResult("best_of_n_attempt: git " + strings.Join(args, " ") + " failed: " + err.Error() + ": " + string(out))
		os.Exit(1)
	}
}

func runScenario(scenario, taskID string) bool {
	handler, ok := scenarioHandlers[scenario]
	if !ok {
		return false
	}
	handler(taskID)
	return true
}

func runSuccess() {
	emitSystem()
	emitAssistant("Working on it...")
	emitResult("Task completed successfully.")
}

// runSuccessThenHang emits a clean terminal result, like runSuccess, but then
// blocks forever instead of exiting — simulating a process that finished its
// work but never exited (e.g. a subagent-spawning skill left CC alive). Used
// to test the watchdog's StopCompletedAgent path, which must reap the
// orphaned process without the completed work being treated as a stall.
func runSuccessThenHang() {
	emitSystem()
	emitAssistant("Working on it...")
	emitResult("Task completed successfully.")
	// A bare `select {}` deadlocks a single-goroutine program instantly (the
	// runtime treats it as a fatal all-goroutines-asleep condition, not a
	// hang); a pending timer keeps the runtime from considering it deadlocked.
	for {
		time.Sleep(time.Hour)
	}
}

// runSybraHomeSentinel reproduces the #1576 wipe scenario for the sybra#1577
// fix, run as a real subprocess through agent.Manager: it writes a marker
// file under its own default SYBRA_HOME (proving the process inherited the
// per-task sandbox, not the operator's real home) then calls bare `sybra-cli
// update` (no --home) on the task, which must reach the real operator store
// through SYBRA_CONTROL_HOME rather than the sandbox.
func runSybraHomeSentinel(taskID string) {
	emitSystem()
	if home := os.Getenv("SYBRA_HOME"); home != "" {
		_ = os.WriteFile(filepath.Join(home, "sentinel-marker"), []byte(taskID), 0o644)
	}
	if taskID != "" {
		runCLI("update", taskID, "--status", "in-review")
	}
	emitResult("Sentinel done.")
}

func runEvaluate(taskID string) {
	emitSystem()
	emitAssistant("Evaluating...")
	if taskID != "" {
		runCLI("update", taskID, "--status", "in-review")
	}
	emitResult("Evaluation complete. Status set to in-review.")
}

// runTestPass / runTestFail emit the adversarial test-runner verdict marker the
// testing-task workflow's route_test_result step branches on.
func runTestPass() {
	emitSystem()
	emitAssistant("Ran the app; every case matched the task.")
	emitResult(testPassReport() + "\nTEST_VERDICT: PASS")
}

func runTestFail() {
	emitSystem()
	emitAssistant("Found a defect against the task.")
	emitResult(testFailureReport() + "\nTEST_VERDICT: FAIL")
}

func runTestInfraFailure() {
	emitSystem()
	emitAssistant("The app could not be exercised in this harness.")
	emitResult(testInfraFailureReport() + "\nTEST_VERDICT: FAIL")
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

func testInfraFailureReport() string {
	return "## Test Failures\n\n" +
		"Classification: infra_failure\n\n" +
		"Requirement tested: the task requires a real manual smoke against the running app.\n\n" +
		"Command run:\n```sh\ncurl http://127.0.0.1:3000/queue\n```\n\n" +
		"Actual output:\n```text\ncurl: (7) Failed to connect to 127.0.0.1 port 3000: Connection refused\n```\n\n" +
		"Expected output: reachable manual-test surface with queued state rendered.\n\n" +
		"Unable to run manual test: isolated harness cannot render the required queued state."
}

func testPassReport() string {
	return "surface_kind: server\n" +
		"app_started: true\n" +
		"start_command: go run ./cmd/test-server\n" +
		"readiness_probe: curl /status\n" +
		"manual_probes:\n" +
		"command: curl /status\n" +
		"expected: expected output\n" +
		"actual: expected output\n" +
		"automated_checks: go test ./internal/sybra => ok\n" +
		"unable_to_run_reason:"
}

// runTestPassVerbose emits a >2000-char summary BEFORE the final-line verdict,
// reproducing a thorough tester's output. The step-output var is truncated to
// 2000 bytes (prefix), so this guards that the engine extracts the verdict from
// the untruncated result instead — a real PASS here must route to ready-pr.
func runTestPassVerbose() {
	emitSystem()
	emitAssistant("Exercising every angle...")
	long := strings.Repeat("Exercised an edge case and the feature held as the task requires. ", 50)
	emitResult(long + "\n" + testPassReport() + "\nTEST_VERDICT: PASS")
}

func runToolResultLarge() {
	emitSystem()
	emitAssistant("Running a large verification command...")
	emitToolUse("toolu_large", "Bash", map[string]any{"command": "go test ./... && golangci-lint run ./..."})

	var b strings.Builder
	for i := range 120 {
		fmt.Fprintf(&b, "ok   github.com/Automaat/sybra/internal/pkg%d 0.0%ds\n", i%5, (i%9)+1)
	}
	b.WriteString("internal/sybra/app.go:217:13: undefined: missingDependency\n")
	b.WriteString("internal/sybra/app.go:217:13: compile failed after refactor\n")
	for i := range 120 {
		fmt.Fprintf(&b, "lint note %03d: checked another file successfully\n", i)
	}
	emitToolResult("toolu_large", b.String(), true)
	emitAssistant("I found the compile failure and corrected it.")
	emitResult("Implementation done. PR created")
}

// runInteractiveImplement emits a full implement result then blocks on stdin
// until EOF, simulating a real conversational claude agent that stays alive
// between turns and exits when the parent closes stdin.
func runInteractiveImplement() {
	emitSystem()
	emitAssistant("Implementing interactively...")
	emitResult("Implementation done. PR created")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// runPRCreated emits an implement result whose text carries a PR URL, so the
// mechanical link-PR step can extract the number via regex.
func runPRCreated() {
	emitSystem()
	emitAssistant("Implementing and pushing PR...")
	emitResult("Implementation done. Created PR https://github.com/test-org/test-repo/pull/42")
}

// runSignalKill emits a start event then kills itself with SIGTERM, simulating
// a container/OS shutdown mid-run.
func runSignalKill() {
	emitSystem()
	emitAssistant("Starting work...")
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {} // block until signal arrives
}

// runHang emits a start event then blocks until the parent kills the process.
// Used in tests where Sybra's StopAgent (cancel ctx → SIGTERM) is the killer,
// distinguishing it from the self-kill in runSignalKill.
func runHang() {
	emitSystem()
	emitAssistant("Hanging...")
	select {} // block until SIGTERM/SIGKILL arrives
}

// runMalformedPROutput emits a long stream of malformed PR-URL fragments
// that exercises the parser's tolerance for noisy implementation output.
func runMalformedPROutput() {
	emitSystem()
	emitAssistant("Implementing and preparing output...")
	var b strings.Builder
	for range 200 {
		b.WriteString("note: saw github.com/test-org/test-repo/pul/42 and github.com/test-org/test-repo/pulls/42\n")
	}
	emitResult(b.String())
}

// runWriteSidecarSuccess serves any workflow step that declares
// import_sidecar/import_sidecars by extracting sybra sidecar paths from
// the rendered prompt and writing a stub there. The engine then ingests it as
// the configured sidecar.
func runWriteSidecarSuccess(taskID string) {
	emitSystem()
	emitAssistant("Writing fake sidecar...")
	for _, path := range extractSidecarPaths(promptArg(os.Args)) {
		_ = os.WriteFile(path, []byte(fakeSidecarContent(path, taskID, "fake-claude")), 0o644)
	}
	emitResult("Sidecar written.")
}

func runRevisePlanSidecars(taskID string) {
	emitSystem()
	emitAssistant("Revising fake plan sidecars...")
	paths := map[string]string{}
	for _, path := range extractSidecarPaths(promptArg(os.Args)) {
		_ = os.WriteFile(path, []byte(fakeSidecarContent(path, taskID, "fake-claude-revision")), 0o644)
		base := filepath.Base(path)
		switch {
		case strings.Contains(base, "plan-research"):
			paths["research"] = path
		case strings.Contains(base, "plan-decisions"):
			paths["decisions"] = path
		case strings.Contains(base, "plan-brief"):
			paths["brief"] = path
		case strings.Contains(base, "plan-contract"):
			paths["contract"] = path
		case strings.Contains(base, "sybra-plan-"):
			paths["plan"] = path
		}
	}
	if taskID != "" {
		var args []string
		if paths["plan"] != "" {
			args = append(args, "--plan-file", paths["plan"])
		}
		if paths["contract"] != "" {
			args = append(args, "--plan-contract-file", paths["contract"])
		}
		if paths["research"] != "" {
			args = append(args, "--plan-research-file", paths["research"])
		}
		if paths["decisions"] != "" {
			args = append(args, "--plan-decisions-file", paths["decisions"])
		}
		if paths["brief"] != "" {
			args = append(args, "--plan-brief-file", paths["brief"])
		}
		if len(args) > 0 {
			runCLI("update", taskID, args...)
		}
		runCLI("update", taskID, "--status", "plan-review")
	}
	emitResult("Revised sidecars and set plan-review.")
}

func runPlanCriticSuccess(taskID string) {
	emitSystem()
	emitAssistant("Critiquing plan...")
	for _, path := range extractSidecarPaths(promptArg(os.Args)) {
		if strings.Contains(filepath.Base(path), "sybra-critique") {
			_ = os.WriteFile(path, []byte("# Plan Critique\n\n## Verdict: REFINE\n\n- Consider edge case X.\n"), 0o644)
		}
	}
	if taskID != "" {
		runCLI("update", taskID, "--plan-critique", "# Plan Critique\n\n## Verdict: REFINE\n\n- Consider edge case X.\n")
	}
	emitResult("Critique saved.")
}

func runPlanCriticNoSave() {
	emitSystem()
	emitAssistant("Blocked by env. Did not save critique.")
	emitResult("Blocked by env.")
}

func runCodeReviewSuccess(taskID string) {
	emitSystem()
	emitAssistant("Reviewing code...")
	if taskID != "" {
		runCLI("update", taskID, "--code-review", "# Code Review\n\nLooks good.\n")
	}
	emitResult("Review saved.")
}

func runTriage(taskID, status, tags string) {
	emitSystem()
	emitAssistant("Triaging task...")
	if taskID != "" {
		if tags != "" {
			runCLI("update", taskID, "--status", status, "--tags", tags)
		} else {
			runCLI("update", taskID, "--status", status)
		}
	}
	msg := "Triage complete. Set status=" + status + "."
	if tags != "" {
		msg = "Triage complete. Set status=" + status + ", tags=" + tags + "."
	}
	emitResult(msg)
}

// runPerfStream emits FAKE_CLAUDE_EVENT_COUNT assistant events at a fixed
// interval, then a result. Used to characterize steady-state throughput.
func runPerfStream() {
	count := envInt("FAKE_CLAUDE_EVENT_COUNT", 100)
	intervalMs := envInt("FAKE_CLAUDE_EVENT_INTERVAL_MS", 10)
	interval := time.Duration(intervalMs) * time.Millisecond
	emitRaw(systemEvent())
	for i := range count {
		emitRaw(assistantEvent(fmt.Sprintf("perf_stream event %d/%d", i+1, count)))
		if interval > 0 {
			time.Sleep(interval)
		}
	}
	emitRaw(resultEvent(fmt.Sprintf("perf_stream emitted %d events", count)))
}

// runPerfBurst emits FAKE_CLAUDE_EVENT_COUNT assistant events with zero
// inter-event sleep, then a result. Used to stress the 50ms emit throttle
// in runner_headless and the downstream event fanout.
func runPerfBurst() {
	count := envInt("FAKE_CLAUDE_EVENT_COUNT", 500)
	emitRaw(systemEvent())
	for i := range count {
		emitRaw(assistantEvent(fmt.Sprintf("perf_burst event %d/%d", i+1, count)))
	}
	emitRaw(resultEvent(fmt.Sprintf("perf_burst emitted %d events", count)))
}

// runPerfLong emits assistant events at a fixed cadence for a total duration,
// then a result. Used for soak / memory-leak detection.
func runPerfLong() {
	durationMs := envInt("FAKE_CLAUDE_DURATION_MS", 30000)
	intervalMs := envInt("FAKE_CLAUDE_EVENT_INTERVAL_MS", 200)
	duration := time.Duration(durationMs) * time.Millisecond
	interval := time.Duration(intervalMs) * time.Millisecond
	emitRaw(systemEvent())
	deadline := time.Now().Add(duration)
	i := 0
	for time.Now().Before(deadline) {
		i++
		emitRaw(assistantEvent(fmt.Sprintf("perf_long event %d", i)))
		if interval > 0 {
			time.Sleep(interval)
		}
	}
	emitRaw(resultEvent(fmt.Sprintf("perf_long emitted %d events over %s", i, duration)))
}

// envInt reads a non-negative integer from env, falling back to def on parse
// error or missing value. Negative or non-integer inputs return def.
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func emitSystem() {
	emit(systemEvent())
}

func emitAssistant(text string) {
	emit(assistantEvent(text))
}

func emitToolUse(id, name string, input map[string]any) {
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": input,
				},
			},
		},
	})
}

func emitToolResult(toolUseID, content string, isError bool) {
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
					"is_error":    isError,
				},
			},
		},
	})
}

func emitResult(result string) {
	emit(resultEvent(result))
}

func runHighCost() {
	emitSystem()
	event := resultEvent("over budget")
	event["total_cost_usd"] = 11.0
	emit(event)
}

func runMalformedToolCallOnce() {
	_ = readInitialPrompt(os.Stdin)
	emitSystem()
	emitToolUse("toolu_1", "functions.exec_command", map[string]any{"yield_time_ms": 1000})
	emitToolResult("toolu_1", malformedToolValidation("missing required property \"cmd\".", `{"type":"object","required":["cmd"]}`), true)
	emitResult("tool call rejected; waiting for correction")
	_ = readInitialPrompt(os.Stdin)
	emitToolUse("toolu_2", "functions.exec_command", map[string]any{"cmd": "pwd", "yield_time_ms": 1000})
	emitToolResult("toolu_2", "/repo\n", false)
	emitResult("corrected tool call succeeded")
}

func runMalformedToolCallRepeat() {
	_ = readInitialPrompt(os.Stdin)
	emitSystem()
	emitToolUse("toolu_1", "functions.exec_command", map[string]any{"yield_time_ms": 1000})
	emitToolResult("toolu_1", malformedToolValidation("missing required property \"cmd\".", `{"type":"object","required":["cmd"]}`), true)
	emitResult("tool call rejected; waiting for correction")
	_ = readInitialPrompt(os.Stdin)
	emitToolUse("toolu_2", "functions.exec_command", map[string]any{"yield_time_ms": 1000})
	emitToolResult("toolu_2", malformedToolValidation("missing required property \"cmd\".", `{"type":"object","required":["cmd"]}`), true)
	emitResult("tool call rejected again")
}

func malformedToolValidation(validationError, schema string) string {
	return "Validation error: " + validationError + "\nExpected schema: " + schema
}

func systemEvent() map[string]any {
	return map[string]any{
		"type":       "system",
		"session_id": "fake-session-1",
	}
}

func assistantEvent(text string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		},
	}
}

func resultEvent(result string) map[string]any {
	return map[string]any{
		"type":                "result",
		"result":              result,
		"session_id":          "fake-session-1",
		"total_cost_usd":      0.01,
		"total_input_tokens":  100.0,
		"total_output_tokens": 50.0,
	}
}

// emit writes an event and sleeps 10ms. Used by legacy scenarios that depend
// on the paced emission for test realism.
func emit(event map[string]any) {
	emitRaw(event)
	time.Sleep(10 * time.Millisecond)
}

// emitRaw writes an event without any post-sleep. Perf scenarios use this so
// they can control cadence explicitly.
func emitRaw(event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
}

// extractTaskID attempts to find a task ID in the -p prompt argument.
// Task IDs look like 8-char hex strings (e.g., "a1b2c3d4").
var taskIDRe = regexp.MustCompile(`\b([a-f0-9]{8})\b`)

func promptArg(args []string) string {
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func extractPrompt(args []string) string {
	return promptTaskID(promptArg(args))
}

func promptTaskID(prompt string) string {
	if matches := taskIDRe.FindStringSubmatch(prompt); len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// sidecarPathRe matches sybra sidecar paths as they appear inside
// rendered workflow prompts. Used by the write_sidecar_success scenario
// so a single fake-agent scenario can serve any import_sidecar step
// without needing per-kind variants.
var sidecarPathRe = regexp.MustCompile(`(?:/tmp/sybra-|\.sybra-)[A-Za-z0-9_./-]+\.(?:md|json)`)

func extractSidecarPaths(prompt string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range sidecarPathRe.FindAllString(prompt, -1) {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
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

// popScenario reads the scenario for this invocation. If FAKE_CLAUDE_SCENARIO_FILE
// is set, it pops the first line from that file (for multi-step workflows).
// Falls back to FAKE_CLAUDE_SCENARIO, then "success".
//
// The read-modify-write of the scenario file is serialized across concurrent
// fake-claude processes via flock(2) on a sibling lockfile. Without this,
// two simultaneous invocations from a multi-task chaos test would observe
// the same first line and both consume it, silently losing scenarios from
// the queue.
func popScenario() string {
	if sf := cleanEnvPath(os.Getenv("FAKE_CLAUDE_SCENARIO_FILE")); sf != "" {
		release, ok := acquireScenarioLock(sf)
		if ok {
			defer release()
		}
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
	if s := os.Getenv("FAKE_CLAUDE_SCENARIO"); s != "" {
		return s
	}
	return "success"
}

// acquireScenarioLock takes an exclusive flock on a sibling .lock file and
// returns a release closure. Returns ok=false on any error so the caller
// continues unlocked rather than failing the test outright (the worst case
// is the original racy behaviour; the alternative — a hard error — would
// hide more bugs than it surfaces).
func acquireScenarioLock(scenarioPath string) (release func(), ok bool) {
	lockPath := scenarioPath + scenarioFileLockSuffix
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

func runCLI(subcmd, taskID string, extra ...string) {
	bin, err := exec.LookPath("sybra-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sybra-cli not found: %v\n", err)
		return
	}
	cmdArgs := []string{"--json", subcmd, taskID}
	cmdArgs = append(cmdArgs, extra...)
	cmd := &exec.Cmd{
		Path:   bin,
		Args:   append([]string{bin}, cmdArgs...),
		Stdout: os.Stderr, // don't pollute NDJSON stdout
		Stderr: os.Stderr,
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "sybra-cli failed: %v\n", err)
	}
}
