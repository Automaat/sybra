package selfmonitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// Judge classifies a single health Finding given its distilled log summary
// and optional task context. Returns a filled Verdict; callers should treat
// errors as transient and keep the existing VerdictPending rather than
// failing the whole tick.
type Judge interface {
	Judge(ctx context.Context, f health.Finding, ls *LogSummary, t *task.Task) (Verdict, error)
}

// ClaudeJudge is the production implementation. It spawns `claude -p` with a
// compact prompt and parses the JSON verdict from the response envelope —
// identical pattern to internal/triage.ClaudeClassifier.
type ClaudeJudge struct {
	Model  string // default: claude-haiku-4-5-20251001
	Logger *slog.Logger
}

// Judge shells out to claude -p and returns a validated Verdict.
func (j *ClaudeJudge) Judge(ctx context.Context, f health.Finding, ls *LogSummary, t *task.Task) (Verdict, error) {
	model := j.Model
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	prompt := buildJudgePrompt(f, ls, t)

	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--model", model,
	)
	out, err := cmd.Output()
	if err != nil {
		return Verdict{Classification: VerdictPending}, fmt.Errorf("claude -p: %w", err)
	}

	v, err := parseJudgeVerdict(out)
	if err != nil {
		return Verdict{Classification: VerdictPending}, fmt.Errorf("parse verdict: %w", err)
	}
	return v, nil
}

func buildJudgePrompt(f health.Finding, ls *LogSummary, t *task.Task) string {
	var b strings.Builder

	b.WriteString("You are diagnosing a Sybra agent workflow finding.\n")
	b.WriteString("Output ONLY a single JSON object on the final line.\n\n")

	b.WriteString("Finding:\n")
	fmt.Fprintf(&b, "  category=%s severity=%s\n", f.Category, f.Severity)
	fmt.Fprintf(&b, "  title=%s\n", f.Title)
	if len(f.Evidence) > 0 {
		ev, _ := json.Marshal(f.Evidence)
		fmt.Fprintf(&b, "  evidence=%s\n", ev)
	}
	b.WriteString("\n")

	if ls != nil {
		b.WriteString("Log analysis:\n")
		fmt.Fprintf(&b, "  totalToolCalls=%d costUSD=%.4f\n", ls.TotalToolCalls, ls.TotalCostUSD)
		if ls.StallDetected {
			fmt.Fprintf(&b, "  STALL: %s\n", ls.StallReason)
		}
		if len(ls.RepeatedCalls) > 0 {
			rc, _ := json.Marshal(ls.RepeatedCalls)
			fmt.Fprintf(&b, "  repeatedCalls=%s\n", rc)
		}
		if len(ls.ErrorClasses) > 0 {
			ec, _ := json.Marshal(ls.ErrorClasses)
			fmt.Fprintf(&b, "  errorClasses=%s\n", ec)
		}
		if len(ls.LastToolCalls) > 0 {
			lc, _ := json.Marshal(ls.LastToolCalls)
			fmt.Fprintf(&b, "  lastToolCalls=%s\n", lc)
		}
		b.WriteString("\n")
	}

	if t != nil {
		fmt.Fprintf(&b, "Task: %s\n", t.Title)
		body := t.Body
		if len(body) > 500 {
			body = body[:500] + "…"
		}
		if body != "" {
			fmt.Fprintf(&b, "Body: %s\n", body)
		}
		b.WriteString("\n")
	}

	b.WriteString(`Output schema (single JSON object, nothing else):
{"classification":"confirmed|false_positive|needs_human","rootCause":"...","evidenceExcerpt":"...","confidence":0.0,"nextAction":"..."}

Rules:
- confirmed: run is genuinely broken or expensive beyond task scope
- false_positive: cost/failure is proportional to task size or complexity
- needs_human: ambiguous, no clear log signal
- evidenceExcerpt: most diagnostic line from the log (stall reason, repeated call, error)
- nextAction: one specific actionable fix — not "investigate further"
`)
	return b.String()
}

// parseJudgeVerdict extracts the verdict from `claude -p --output-format json`
// stdout. The envelope has a `result` string field; we extract the last JSON
// object from it.
func parseJudgeVerdict(raw []byte) (Verdict, error) {
	var envelope struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Verdict{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if envelope.Result == "" {
		return Verdict{}, fmt.Errorf("empty result field")
	}
	jsonStr := judgeExtractLastJSON(envelope.Result)
	if jsonStr == "" {
		return Verdict{}, fmt.Errorf("no JSON object in result: %q", envelope.Result)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return Verdict{}, fmt.Errorf("unmarshal verdict: %w", err)
	}
	if v.Classification == "" {
		v.Classification = VerdictPending
	}
	return v, nil
}

// judgeExtractLastJSON returns the last balanced {...} substring in s, or "".
// Mirrors triage.extractLastJSONObject and internal/agent/inspector.go.
func judgeExtractLastJSON(s string) string {
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
