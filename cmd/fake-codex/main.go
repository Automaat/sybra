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
	case "fail_exit":
		emitError("command failed")
		os.Exit(1)
	case "no_result":
		emitAgentMessage("Working on it...")
		os.Exit(0)
	case "triage":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "todo", "--tags", "small")
		emitTurnCompleted(100, 20)
	case "triage_to_planning":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "planning", "--tags", "large")
		emitTurnCompleted(100, 20)
	case "triage_to_planning_nocritic":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "planning", "--tags", "large,nocritic")
		emitTurnCompleted(100, 20)
	case "plan_critic_success":
		emitAgentMessage("Critiquing plan...")
		runCLI(taskID, "update", taskID, "--plan-critique", "# Plan Critique\n\n## Verdict: REFINE\n\n- Consider edge case X.\n")
		emitTurnCompleted(100, 20)
	case "plan_critic_no_save":
		// Simulates codex agent blocked by env (bwrap sandbox failure) — exits
		// cleanly without saving the critique sidecar.
		emitAgentMessage("Blocked by env. Did not save critique.")
		emitTurnCompleted(100, 20)
	case "code_review_success":
		emitAgentMessage("Reviewing code...")
		runCLI(taskID, "update", taskID, "--code-review", "# Code Review\n\nLooks good.\n")
		emitTurnCompleted(100, 20)
	case "triage_to_done":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "done")
		emitTurnCompleted(100, 20)
	case "triage_to_in_review":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "in-review")
		emitTurnCompleted(100, 20)
	case "triage_to_human_required":
		emitAgentMessage("Triaging task...")
		runCLI(taskID, "update", taskID, "--status", "human-required")
		emitTurnCompleted(100, 20)
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
		var b strings.Builder
		for range 200 {
			b.WriteString("note: saw github.com/test-org/test-repo/pul/42 and github.com/test-org/test-repo/pulls/42\n")
		}
		emitAgentMessage(b.String())
		emitTurnCompleted(100, 20)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario: %s\n", scenario)
		os.Exit(2)
	}
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
