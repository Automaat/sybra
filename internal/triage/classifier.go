package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
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

// FallbackClassifier runs triage through the shared provider-fallback executor.
type FallbackClassifier struct {
	Model  string
	Logger *slog.Logger
	Gate   provider.HealthGate
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

// Classify shells out to the first available provider and falls back when it is
// rate-limited/logged-out/unavailable.
func (c *FallbackClassifier) Classify(ctx context.Context, t task.Task, projects []project.Project) (Verdict, error) {
	v, _, err := llmjob.Run(ctx, buildPrompt(t, projects), llmjob.Spec[Verdict]{
		Name:     "triage",
		Tier:     llmjob.Cheap,
		Validate: ValidateVerdict,
	}, llmexec.Options{
		Logger: c.Logger,
		Gate:   c.Gate,
	})
	if err != nil {
		return Verdict{}, err
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
- tags: pick from: backend, frontend, infra, docs, ci, auth, db, test. Also include one of small|medium|large and one of bug|feature|refactor|review|chore|docs — 2-5 of these vocabulary tags. Separately, add the routing tag "noplan" when the task qualifies (see the noplan guide below): it is the only tag outside the lists above you may emit, and it does NOT count toward the 2-5 — never drop a deserved "noplan" to stay under the cap.
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

Decision guide for noplan (skip the planning phase — go straight to implementation):
- Add "noplan" ONLY when the task is small AND trivially mechanical: the fix is
  obvious and needs no design decisions or up-front scoping.
- Good fits: dependency/version bumps, lockfile regeneration, CI/lint/config
  tweaks, fixing a red CI check on a Renovate PR, typo/comment/docstring fixes,
  a small mechanical rename.
- Do NOT add "noplan" when the approach is non-obvious, scope is unclear, type is
  feature, or the change touches a public API, data model, auth, or concurrency.
- When in doubt, omit it — planning is the safe default.

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

	// Expose system metadata so the classifier can recognise pr-fix tasks and
	// emit "noplan" without depending solely on title/body heuristics.
	if t.RunRole != "" || t.PRNumber > 0 {
		b.WriteString("System metadata (do not include in output):\n")
		if t.RunRole != "" {
			b.WriteString("- run_role: " + t.RunRole + "\n")
		}
		if t.PRNumber > 0 {
			fmt.Fprintf(&b, "- pr_number: %d\n", t.PRNumber)
		}
		b.WriteString("IMPORTANT: when run_role=pr-fix or pr_number>0 this is a system task " +
			"fixing an existing PR. Always emit \"noplan\" in tags.\n\n")
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
	text := string(raw)
	var envelope struct {
		Result *string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != nil {
		if *envelope.Result == "" {
			return Verdict{}, fmt.Errorf("empty result field")
		}
		text = *envelope.Result
	}
	jsonStr := extractLastJSONObject(text)
	if jsonStr == "" {
		return Verdict{}, fmt.Errorf("no JSON object in result: %q", text)
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
