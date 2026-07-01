package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
)

// InspectorVerdict is the structured judgment returned by the inspector agent.
type InspectorVerdict struct {
	Stuck          bool   `json:"stuck"`
	Reason         string `json:"reason"`
	Recommendation string `json:"recommendation"` // "stop" | "continue" | "escalate" | "nudge"
	// Nudge is the short corrective message the supervisor should deliver to the
	// agent when Recommendation == "nudge". Empty for other recommendations.
	Nudge string `json:"nudge,omitempty"`
}

// InspectInput holds the context handed to the inspector about the target agent.
type InspectInput struct {
	AgentID   string
	TaskTitle string
	LogPath   string
	StallSec  int
	TotalSec  int
	// Trigger names why inspection fired ("stall", "budget", or "loop"), so the
	// prompt can focus the judge. Empty is treated as a generic stall check.
	Trigger string
	// Model selects the judge model (e.g. a cheap "haiku" for routine checks).
	// Empty falls back to "sonnet" for backwards compatibility.
	Model string
}

// Inspect spawns `claude -p` to analyze a running agent's NDJSON log and return
// a verdict on whether it appears stuck. The caller must supply a context with
// a reasonable timeout (e.g. 2 minutes).
// The inspector always runs with --dangerously-skip-permissions because it is
// short-lived and read-only; logger receives a warning on each invocation.
func Inspect(ctx context.Context, logger *slog.Logger, in InspectInput) (InspectorVerdict, error) {
	logger.Warn("inspector: running with --dangerously-skip-permissions",
		"agent_id", in.AgentID, "task_title", in.TaskTitle)

	prompt := buildInspectorPrompt(in)

	v, _, err := llmjob.Run(ctx, prompt, llmjob.Spec[InspectorVerdict]{
		Name:     "inspect",
		Tier:     llmjob.Cheap,
		Validate: validateInspectorVerdict,
	}, llmexec.Options{Logger: logger, Models: claudeModelOverride(in.Model)})
	if err != nil {
		return InspectorVerdict{}, fmt.Errorf("inspector: %w", err)
	}
	return v, nil
}

func claudeModelOverride(model string) map[string]string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return map[string]string{"claude": model}
}

func validateInspectorVerdict(v *InspectorVerdict) error {
	switch v.Recommendation {
	case "stop", "continue", "escalate", "nudge":
		return nil
	default:
		return fmt.Errorf("invalid recommendation: %q", v.Recommendation)
	}
}

func buildInspectorPrompt(in InspectInput) string {
	trigger := in.Trigger
	if trigger == "" {
		trigger = "stall"
	}
	return fmt.Sprintf(`You are a watchdog inspecting a running Claude Code agent that may be stuck.

Agent ID: %s
Task: %s
Inspection trigger: %s
Time since last stream event: %d seconds
Total runtime: %d seconds
NDJSON log path: %s

Read the log file (last ~200 lines are most relevant). Look for:
- Repeating tool calls with identical arguments
- Same reasoning/text being repeated
- Thrashing between the same files or commands
- No forward progress toward the task goal

Output ONLY a single JSON object on the final line, nothing else:
{"stuck": bool, "reason": "short explanation", "recommendation": "stop"|"continue"|"escalate"|"nudge", "nudge": "corrective steer (only when recommendation is nudge)"}

Recommendations:
- "stop": agent is clearly looping/stuck with no recovery, kill it
- "nudge": agent is drifting but recoverable — set "nudge" to a one-sentence
  steer that redirects it (e.g. "stop retrying the failing command; read the
  error and fix the root cause first")
- "escalate": ambiguous or needs human judgment, flag for human
- "continue": agent is making progress, leave it alone`,
		in.AgentID, in.TaskTitle, trigger, in.StallSec, in.TotalSec, in.LogPath)
}

// parseInspectorOutput extracts the verdict from `claude -p --output-format json` stdout.
// The top-level response has a `result` string field containing the model's final message,
// from which we extract the last JSON object.
func parseInspectorOutput(raw []byte) (InspectorVerdict, error) {
	text := string(raw)
	var envelope struct {
		Result *string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != nil {
		if *envelope.Result == "" {
			return InspectorVerdict{}, fmt.Errorf("empty result field")
		}
		text = *envelope.Result
	}
	jsonStr := extractLastJSONObject(text)
	if jsonStr == "" {
		return InspectorVerdict{}, fmt.Errorf("no JSON object in result: %q", text)
	}
	var v InspectorVerdict
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return InspectorVerdict{}, fmt.Errorf("unmarshal verdict: %w", err)
	}
	switch v.Recommendation {
	case "stop", "continue", "escalate", "nudge":
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
	for i := range len(s) {
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
