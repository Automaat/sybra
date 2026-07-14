package triage

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/llmjob"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

// Classifier produces a triage verdict for a task. Exposed as an interface so
// tests can inject a canned verdict without a live provider call.
type Classifier interface {
	Classify(ctx context.Context, t task.Task, projects []project.Project) (Verdict, error)
}

// FallbackClassifier runs triage through the shared provider-fallback executor.
type FallbackClassifier struct {
	Model  string
	Logger *slog.Logger
	Gate   provider.HealthGate
}

// Classify shells out to the first available provider and falls back when it is
// rate-limited/logged-out/unavailable.
func (c *FallbackClassifier) Classify(ctx context.Context, t task.Task, projects []project.Project) (Verdict, error) {
	v, _, err := llmjob.Run(ctx, buildPrompt(t, projects), llmjob.Spec[Verdict]{
		Name:     "triage",
		Tier:     llmjob.SuperCheap,
		Schema:   Schema,
		Validate: ValidateVerdict,
	}, llmexec.Options{
		Logger: c.Logger,
		Gate:   c.Gate,
		Models: claudeModelOverride(c.Model),
	})
	if err != nil {
		return Verdict{}, err
	}
	return v, nil
}

func claudeModelOverride(model string) map[string]string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return map[string]string{"claude": model}
}

func buildPrompt(t task.Task, projects []project.Project) string {
	var b strings.Builder
	b.WriteString(`You are triaging a task from a task-management system.
Classify it into a strict JSON verdict. Output ONLY a single JSON object on the final line, nothing else before or after.

Rules:
- title: ALWAYS rewrite into a clean, human-readable, imperative conventional-commit-style title (e.g. "feat(auth): add JWT middleware", "fix(api): handle nil pointer on empty body"). Even if the input title already looks fine, produce your best version. Max 80 chars.
- original_title: copy the input title verbatim so the user can recover it later.
- description: ONLY set if the input body is empty/just-a-URL. 2-3 sentences describing what the task is about and what "done" looks like. Otherwise leave empty string.
- tags: pick from: backend, frontend, infra, docs, ci, auth, db, test. Also include one of small|medium|large and one of bug|feature|refactor|review|chore|docs — 2-5 of these vocabulary tags. Separately, add the routing tags "noplan" and/or "trivial" when the task qualifies (see the noplan/trivial guide below): these are the only tags outside the lists above you may emit, and they do NOT count toward the 2-5 — never drop a deserved "noplan"/"trivial" to stay under the cap.
- size: small|medium|large
- type: bug|feature|refactor|review|chore|docs
- mode: headless (automated, no human-in-the-loop needed) or interactive (needs human judgment during execution)
- project_id: if the task title or body contains a github.com URL matching one of the registered projects below, set this to that project's "owner/repo". Otherwise empty string. If the "System metadata" section below already shows an existing_project_id or issue_url resolving to a registered project, leave project_id empty — the system already knows the answer and will not use your guess to override it. Only ever set this from a clear github.com URL, never from topical/vocabulary similarity to a project's name.

Decision guide for mode:
- PR review, simple fix, test writing, refactor → headless
- Architecture decision, unclear scope, complex debugging → interactive

Decision guide for size:
- small: <50 LOC, single file, trivial
- medium: multiple files, clear scope, design mostly known
- large: cross-cutting, new subsystem, or unclear scope

Decision guide for noplan (skip the planning phase — go straight to implementation)
and trivial (also skip agentic code review AND adversarial manual testing —
straight to opening the PR after implementation):
- Add "noplan" and/or "trivial" ONLY when the task is small AND trivially
  mechanical: the fix is obvious and needs no design decisions or up-front
  scoping. They are independent — add both when the task qualifies for both,
  or just "noplan" if you want a human/reviewer to still see the diff.
- Good fits for either: dependency/version bumps, lockfile regeneration,
  CI/lint/config tweaks, fixing a red CI check on a Renovate PR, typo/comment/
  docstring fixes, a small mechanical rename.
- Do NOT add "noplan" or "trivial" when the approach is non-obvious, scope is
  unclear, type is feature, or the change touches a public API, data model,
  auth, or concurrency.
- Never add "trivial" when type is bug — trivial skips both review and
  testing, and a "small" bug fix is exactly the case where a subtle regression
  can slip past both. "noplan" alone is still fine for a trivial bug fix.
- When in doubt, omit both — planning, review, and testing are the safe
  default.

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
	// emit "noplan" without depending solely on title/body heuristics, and so
	// it has an anchor for project_id instead of guessing from vocabulary
	// overlap with a registered project's domain.
	if t.RunRole != "" || t.PRNumber > 0 || t.ProjectID != "" || t.Issue != "" {
		b.WriteString("System metadata (do not include in output):\n")
		if t.RunRole != "" {
			b.WriteString("- run_role: " + t.RunRole + "\n")
		}
		if t.PRNumber > 0 {
			fmt.Fprintf(&b, "- pr_number: %d\n", t.PRNumber)
		}
		if t.ProjectID != "" {
			b.WriteString("- existing_project_id: " + t.ProjectID + "\n")
		}
		if t.Issue != "" {
			b.WriteString("- issue_url: " + t.Issue + "\n")
		}
		if t.RunRole != "" || t.PRNumber > 0 {
			b.WriteString("IMPORTANT: when run_role=pr-fix or pr_number>0 this is a system task " +
				"fixing an existing PR. Always emit \"noplan\" in tags.\n")
		}
		if t.ProjectID != "" || t.Issue != "" {
			b.WriteString("IMPORTANT: existing_project_id and/or issue_url above are already " +
				"authoritative for this task's project. Leave project_id empty in your output — " +
				"the system resolves it from these fields, not your guess.\n")
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
