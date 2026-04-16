package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Classifier wraps the claude -p invocation. Exposed as an interface so
// tests can inject a canned verdict without shelling out.
type Classifier interface {
	Classify(ctx context.Context, t task.Task, projects []project.Project) (Verdict, error)
}

// ClaudeClassifier is the production implementation. It spawns `claude -p`
// with a strict JSON schema prompt and parses the envelope.
type ClaudeClassifier struct {
	Model  string       // default: "sonnet"
	Logger *slog.Logger // required
}

// Classify shells out to claude -p and returns a validated verdict.
func (c *ClaudeClassifier) Classify(ctx context.Context, t task.Task, projects []project.Project) (Verdict, error) {
	model := c.Model
	if model == "" {
		model = "sonnet"
	}

	prompt := buildPrompt(t, projects)

	cmd := exec.CommandContext(ctx, "claude",
		"-p", prompt,
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--model", model,
	)
	out, err := cmd.Output()
	if err != nil {
		return Verdict{}, fmt.Errorf("claude -p: %w", err)
	}

	v, err := parseVerdict(out)
	if err != nil {
		return Verdict{}, fmt.Errorf("parse verdict: %w", err)
	}
	if err := ValidateVerdict(&v); err != nil {
		return Verdict{}, fmt.Errorf("validate verdict: %w", err)
	}
	return v, nil
}

func buildPrompt(t task.Task, projects []project.Project) string {
	var b strings.Builder
	b.WriteString(`You are triaging a task from a task-management system.
Classify it into a strict JSON verdict. Output ONLY a single JSON object on the final line, nothing else before or after.

Rules:
- title: ALWAYS rewrite into a clean, human-readable, imperative conventional-commit-style title (e.g. "feat(auth): add JWT middleware", "fix(api): handle nil pointer on empty body"). Even if the input title already looks fine, produce your best version. Max 80 chars.
- original_title: copy the input title verbatim so the user can recover it later.
- description: ONLY set if the input body is empty/just-a-URL. 2-3 sentences describing what the task is about and what "done" looks like. Otherwise leave empty string.
- tags: pick from: backend, frontend, infra, docs, ci, auth, db, test. Also include one of small|medium|large and one of bug|feature|refactor|review|chore|docs. 2-5 tags total.
- size: small|medium|large
- type: bug|feature|refactor|review|chore|docs
- mode: headless (automated, no human-in-the-loop needed) or interactive (needs human judgment during execution)
- project_id: if the task title or body contains a github.com URL matching one of the registered projects below, set this to that project's "owner/repo". Otherwise empty string.

Decision guide for mode:
- PR review, simple fix, test writing, refactor → headless
- Architecture decision, unclear scope, complex debugging → interactive

Decision guide for size:
- small: <50 LOC, single file, trivial
- medium: multiple files, clear scope, design mostly known
- large: cross-cutting, new subsystem, or unclear scope

Output schema (single JSON object):
{"title":"...","original_title":"...","description":"","tags":["..."],"size":"small","type":"feature","mode":"headless","project_id":""}

`)

	if len(projects) > 0 {
		b.WriteString("Registered projects:\n")
		for i := range projects {
			b.WriteString("- ")
			b.WriteString(projects[i].ID)
			b.WriteString(" (")
			b.WriteString(string(projects[i].Type))
			b.WriteString(")\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("Task to classify:\n")
	b.WriteString("TITLE: ")
	b.WriteString(t.Title)
	b.WriteString("\nBODY:\n")
	if strings.TrimSpace(t.Body) == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(t.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// parseVerdict extracts the verdict from `claude -p --output-format json` stdout.
// The top-level response has a `result` string field containing the model's
// final message, from which we extract the last JSON object.
func parseVerdict(raw []byte) (Verdict, error) {
	var envelope struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Verdict{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if envelope.Result == "" {
		return Verdict{}, fmt.Errorf("empty result field")
	}
	jsonStr := extractLastJSONObject(envelope.Result)
	if jsonStr == "" {
		return Verdict{}, fmt.Errorf("no JSON object in result: %q", envelope.Result)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return Verdict{}, fmt.Errorf("unmarshal verdict: %w", err)
	}
	return v, nil
}

// extractLastJSONObject returns the last balanced {...} substring in s, or "".
// Mirrors internal/agent/inspector.go's helper. Tracks string-literal state
// so braces inside string values don't count toward depth.
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
