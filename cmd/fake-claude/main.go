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
//   - triage: runs synapse-cli to set status=todo, tags=small, emits result
//   - triage_to_planning: runs synapse-cli to set status=planning, tags=large
//   - triage_to_planning_nocritic: like triage_to_planning but adds nocritic tag
//   - implement: emits result with "PR created" text
//   - interactive_implement: emits result then blocks on stdin until EOF,
//     simulating a real conversational claude agent that stays alive between
//     turns. Exits when the parent closes stdin — e.g. the one-shot runner
//     path that closes stdin after the first result event.
//   - evaluate: runs synapse-cli to set status=in-review, emits result
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func main() {
	// Log args for test verification.
	if logFile := os.Getenv("FAKE_CLAUDE_ARGS_LOG"); logFile != "" {
		_ = os.WriteFile(logFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	}

	scenario := popScenario()

	taskID := extractTaskID(os.Args)

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
		emitSystem()
		emitAssistant("Triaging task...")
		if taskID != "" {
			runCLI("update", taskID, "--status", "todo", "--tags", "small")
		}
		emitResult("Triage complete. Set status=todo, tags=small.")

	case "triage_to_planning":
		emitSystem()
		emitAssistant("Triaging task...")
		if taskID != "" {
			runCLI("update", taskID, "--status", "planning", "--tags", "large")
		}
		emitResult("Triage complete. Set status=planning, tags=large.")

	case "triage_to_planning_nocritic":
		emitSystem()
		emitAssistant("Triaging task...")
		if taskID != "" {
			runCLI("update", taskID, "--status", "planning", "--tags", "large,nocritic")
		}
		emitResult("Triage complete. Set status=planning, tags=large,nocritic.")

	case "implement":
		emitSystem()
		emitAssistant("Implementing...")
		emitResult("Implementation done. PR created")

	case "interactive_implement":
		emitSystem()
		emitAssistant("Implementing interactively...")
		emitResult("Implementation done. PR created")
		// Block on stdin so we mirror a real conversational claude agent
		// that waits for more input between turns. The runner with
		// OneShot=true closes our stdin after reading the result event,
		// unblocking this read and letting the process exit — which is
		// exactly the signal path the one-shot fix relies on.
		_, _ = io.Copy(io.Discard, os.Stdin)

	case "evaluate":
		emitSystem()
		emitAssistant("Evaluating...")
		if taskID != "" {
			runCLI("update", taskID, "--status", "in-review")
		}
		emitResult("Evaluation complete. Status set to in-review.")

	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}
}

func emitSystem() {
	emit(map[string]any{
		"type":       "system",
		"session_id": "fake-session-1",
	})
}

func emitAssistant(text string) {
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": text},
			},
		},
	})
}

func emitResult(result string) {
	emit(map[string]any{
		"type":                "result",
		"result":              result,
		"session_id":          "fake-session-1",
		"total_cost_usd":      0.01,
		"total_input_tokens":  100.0,
		"total_output_tokens": 50.0,
	})
}

func emit(event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
	time.Sleep(10 * time.Millisecond)
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
	if sf := os.Getenv("FAKE_CLAUDE_SCENARIO_FILE"); sf != "" {
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
	bin, err := exec.LookPath("synapse-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "synapse-cli not found: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "synapse-cli failed: %v\n", err)
	}
}
