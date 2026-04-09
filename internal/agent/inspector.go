package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// InspectorVerdict is the structured judgment returned by the inspector agent.
type InspectorVerdict struct {
	Stuck          bool   `json:"stuck"`
	Reason         string `json:"reason"`
	Recommendation string `json:"recommendation"` // "stop" | "continue" | "escalate"
}

// InspectInput holds the context handed to the inspector about the target agent.
type InspectInput struct {
	AgentID   string
	TaskTitle string
	LogPath   string
	StallSec  int
	TotalSec  int
}

// Inspect spawns `claude -p` to analyze a running agent's NDJSON log and return
// a verdict on whether it appears stuck. The caller must supply a context with
// a reasonable timeout (e.g. 2 minutes).
func Inspect(ctx context.Context, in InspectInput) (InspectorVerdict, error) {
	prompt := buildInspectorPrompt(in)

	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--model", "sonnet",
	)
	out, err := cmd.Output()
	if err != nil {
		return InspectorVerdict{}, fmt.Errorf("inspector claude: %w", err)
	}
	return parseInspectorOutput(out)
}

func buildInspectorPrompt(in InspectInput) string {
	return fmt.Sprintf(`You are a watchdog inspecting a running Claude Code agent that may be stuck.

Agent ID: %s
Task: %s
Stalled for: %d seconds (no new stream events)
Total runtime: %d seconds
NDJSON log path: %s

Read the log file (last ~200 lines are most relevant). Look for:
- Repeating tool calls with identical arguments
- Same reasoning/text being repeated
- Thrashing between the same files or commands
- No forward progress toward the task goal

Output ONLY a single JSON object on the final line, nothing else:
{"stuck": bool, "reason": "short explanation", "recommendation": "stop"|"continue"|"escalate"}

Recommendations:
- "stop": agent is clearly looping/stuck, kill it
- "escalate": ambiguous or needs human judgment, flag for human
- "continue": agent is making progress, leave it alone`,
		in.AgentID, in.TaskTitle, in.StallSec, in.TotalSec, in.LogPath)
}

// parseInspectorOutput extracts the verdict from `claude -p --output-format json` stdout.
// The top-level response has a `result` string field containing the model's final message,
// from which we extract the last JSON object.
func parseInspectorOutput(raw []byte) (InspectorVerdict, error) {
	var envelope struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return InspectorVerdict{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if envelope.Result == "" {
		return InspectorVerdict{}, fmt.Errorf("empty result field")
	}
	jsonStr := extractLastJSONObject(envelope.Result)
	if jsonStr == "" {
		return InspectorVerdict{}, fmt.Errorf("no JSON object in result: %q", envelope.Result)
	}
	var v InspectorVerdict
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return InspectorVerdict{}, fmt.Errorf("unmarshal verdict: %w", err)
	}
	switch v.Recommendation {
	case "stop", "continue", "escalate":
	default:
		return InspectorVerdict{}, fmt.Errorf("invalid recommendation: %q", v.Recommendation)
	}
	return v, nil
}

// extractLastJSONObject returns the last balanced {...} substring in s, or "".
// It tracks JSON string-literal state so that braces inside string values
// are not counted toward depth.
func extractLastJSONObject(s string) string {
	s = strings.TrimSpace(s)
	var (
		inString  bool
		escape    bool
		depth     int
		objStart  = -1
		lastStart = -1
		lastEnd   = -1
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				objStart = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && objStart >= 0 {
				lastStart = objStart
				lastEnd = i
				objStart = -1
			}
		}
	}
	if lastStart < 0 {
		return ""
	}
	return s[lastStart : lastEnd+1]
}
