// Package prcontent drafts the title/body for a Sybra-opened pull request.
// It is the one remaining LLM call in the create_pr tail — everything else
// (push, gh pr create, closing-issue linking, attribution) is deterministic
// Go, wired through internal/workflow's step handlers.
package prcontent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
	"github.com/Automaat/sybra/internal/provider"
)

// Request is the material available to draft a PR title/body from.
type Request struct {
	TaskTitle      string
	TaskBody       string
	CommitSubjects []string
}

// Content is the drafted title/body. Body never includes a "Closes <issue>"
// reference or the harness attribution footer — both are appended
// deterministically by later workflow steps (ensure_pr_closes_issue,
// stamp_pr_attribution).
type Content struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Generator drafts PR content for a task.
type Generator interface {
	Generate(ctx context.Context, req Request) (Content, error)
}

// FallbackGenerator runs the draft job through the shared provider-fallback
// executor (claude -> codex -> copilot), mirroring triage.FallbackClassifier.
type FallbackGenerator struct {
	Model  string
	Logger *slog.Logger
	Gate   provider.HealthGate
}

// Generate drafts a conventional-commit-style title and a Motivation +
// Implementation information body for the given task.
func (g *FallbackGenerator) Generate(ctx context.Context, req Request) (Content, error) {
	c, _, err := llmjob.Run(ctx, buildPrompt(req), llmjob.Spec[Content]{
		Name:     "pr-content",
		Tier:     llmjob.Cheap,
		Validate: validateContent,
	}, llmexec.Options{
		Logger: g.Logger,
		Gate:   g.Gate,
		Models: claudeModelOverride(g.Model),
	})
	if err != nil {
		return Content{}, err
	}
	return c, nil
}

func validateContent(c *Content) error {
	c.Title = strings.TrimSpace(c.Title)
	c.Body = strings.TrimSpace(c.Body)
	if c.Title == "" {
		return fmt.Errorf("title is empty")
	}
	if len(c.Title) > 80 {
		return fmt.Errorf("title too long (%d chars, max 80)", len(c.Title))
	}
	if !strings.Contains(c.Body, "## Motivation") {
		return fmt.Errorf("body missing '## Motivation' section")
	}
	if !strings.Contains(c.Body, "## Implementation information") {
		return fmt.Errorf("body missing '## Implementation information' section")
	}
	return nil
}

func claudeModelOverride(model string) map[string]string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return map[string]string{"claude": model}
}

func buildPrompt(req Request) string {
	var b strings.Builder
	b.WriteString(`You are drafting a GitHub pull request title and body for a task that already passed code review and manual testing. Output ONLY a single JSON object on the final line, nothing else before or after.

Rules:
- title: conventional-commit format ("type(scope): description"), imperative mood, matching the actual work done. Max 80 chars.
- body: markdown with exactly two sections, "## Motivation" and "## Implementation information" (these exact headings). Motivation explains why the change was made; Implementation information summarizes what changed, in prose or bullets.
- Never include a "Closes #N" line or any harness/attribution footer — those are added separately.
- Base the content on the task description and the commit subjects below, not on assumptions.

Schema: {"title": "string", "body": "string"}

`)
	b.WriteString("Task title: ")
	b.WriteString(req.TaskTitle)
	b.WriteString("\n\nTask body:\n")
	b.WriteString(req.TaskBody)
	if len(req.CommitSubjects) > 0 {
		b.WriteString("\n\nCommits on this branch:\n")
		for i, s := range req.CommitSubjects {
			b.WriteString(strconv.Itoa(i + 1))
			b.WriteString(". ")
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}
