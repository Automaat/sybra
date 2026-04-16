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
//   - auth_error: emits auth-failure text then exits 1
//   - malformed_pr_output: emits large malformed PR-ish text (no valid URL)
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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	// Log args for test verification.
	if logFile := cleanEnvPath(os.Getenv("FAKE_CLAUDE_ARGS_LOG")); logFile != "" {
		_ = os.WriteFile(logFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}

	scenario := popScenario()
	if !runScenario(scenario, extractTaskID(os.Args)) {
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}
}

func runScenario(scenario, taskID string) bool {
	switch scenario {
	case "success":
		emitSystem()
		emitAssistant("Working on it...")
		emitResult("Task completed successfully.")
	case "fail_exit":
		emitSystem()
		os.Exit(1)
	case "no_result":
		emitSystem()
		emitAssistant("Working on it...")
		os.Exit(0)
	case "triage":
		runTriage(taskID, "todo", "small")
	case "triage_to_planning":
		runTriage(taskID, "planning", "large")
	case "triage_to_planning_nocritic":
		runTriage(taskID, "planning", "large,nocritic")
	case "plan_critic_success":
		emitSystem()
		emitAssistant("Critiquing plan...")
		if taskID != "" {
			runCLI("update", taskID, "--plan-critique", "# Plan Critique\n\n## Verdict: REFINE\n\n- Consider edge case X.\n")
		}
		emitResult("Critique saved.")
	case "plan_critic_no_save":
		// Simulates codex agent that exited cleanly without actually saving
		// the critique sidecar (bwrap-blocked shell inside Docker container).
		emitSystem()
		emitAssistant("Blocked by env. Did not save critique.")
		emitResult("Blocked by env.")
	case "code_review_success":
		emitSystem()
		emitAssistant("Reviewing code...")
		if taskID != "" {
			runCLI("update", taskID, "--code-review", "# Code Review\n\nLooks good.\n")
		}
		emitResult("Review saved.")
	case "triage_to_done":
		runTriage(taskID, "done", "")
	case "triage_to_in_review":
		runTriage(taskID, "in-review", "")
	case "triage_to_human_required":
		runTriage(taskID, "human-required", "")
	case "implement":
		emitSystem()
		emitAssistant("Implementing...")
		emitResult("Implementation done. PR created")
	case "interactive_implement":
		emitSystem()
		emitAssistant("Implementing interactively...")
		emitResult("Implementation done. PR created")
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "evaluate":
		emitSystem()
		emitAssistant("Evaluating...")
		if taskID != "" {
			runCLI("update", taskID, "--status", "in-review")
		}
		emitResult("Evaluation complete. Status set to in-review.")
	case "pr_created":
		emitSystem()
		emitAssistant("Implementing and pushing PR...")
		emitResult("Implementation done. Created PR https://github.com/test-org/test-repo/pull/42")
	case "auth_error":
		emitSystem()
		emitAssistant("Authentication failed. Please re-auth.")
		os.Exit(1)
	case "malformed_pr_output":
		emitSystem()
		emitAssistant("Implementing and preparing output...")
		var b strings.Builder
		for range 200 {
			b.WriteString("note: saw github.com/test-org/test-repo/pul/42 and github.com/test-org/test-repo/pulls/42\n")
		}
		emitResult(b.String())
	case "perf_stream":
		runPerfStream()
	case "perf_burst":
		runPerfBurst()
	case "perf_long":
		runPerfLong()
	default:
		return false
	}
	return true
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

func emitResult(result string) {
	emit(resultEvent(result))
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

func extractTaskID(args []string) string {
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) {
			if matches := taskIDRe.FindStringSubmatch(args[i+1]); len(matches) > 1 {
				return matches[1]
			}
		}
	}
	return ""
}

// popScenario reads the scenario for this invocation. If FAKE_CLAUDE_SCENARIO_FILE
// is set, it pops the first line from that file (for multi-step workflows).
// Falls back to FAKE_CLAUDE_SCENARIO, then "success".
func popScenario() string {
	if sf := cleanEnvPath(os.Getenv("FAKE_CLAUDE_SCENARIO_FILE")); sf != "" {
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
