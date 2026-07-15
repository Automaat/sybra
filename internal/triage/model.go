// Package triage classifies incoming tasks into a structured verdict
// (title, tags, size/type, mode, project) using a single claude -p call
// and applies the result atomically via task.Manager.UpdateMap.
package triage

import (
	"fmt"
	"slices"
	"strings"
)

// Verdict is the structured classification returned by the LLM.
type Verdict struct {
	Title         string   `json:"title"`
	Description   string   `json:"description,omitempty"`
	Tags          []string `json:"tags"`
	Size          string   `json:"size"`
	Type          string   `json:"type"`
	Mode          string   `json:"mode"`
	ProjectID     string   `json:"project_id,omitempty"`
	OriginalTitle string   `json:"original_title,omitempty"`
}

// Schema is the JSON Schema passed as llmjob.Spec.Schema for FallbackClassifier
// runs — embedded in the prompt for claude/copilot, delivered via codex's
// --output-schema temp file for codex (see llmexec.RunJSON) — mirroring
// internal/verdict.Schema's pattern. Optional fields (description,
// project_id, original_title) are modeled as nullable and listed as required
// so Codex's strict additionalProperties:false rule is satisfied — the model
// emits null for them when unset, which json.Unmarshal decodes into the zero
// value, matching the `omitempty` prose-parsing path's behavior.
const Schema = `{"type":"object","properties":{"title":{"type":"string"},"description":{"type":["string","null"]},"tags":{"type":"array","items":{"type":"string"}},"size":{"type":"string","enum":["small","medium","large"]},"type":{"type":"string","enum":["bug","feature","refactor","review","chore","docs"]},"mode":{"type":"string","enum":["headless","interactive"]},"project_id":{"type":["string","null"]},"original_title":{"type":["string","null"]}},"required":["title","description","tags","size","type","mode","project_id","original_title"],"additionalProperties":false}`

var (
	validSizes = []string{"small", "medium", "large"}
	validTypes = []string{"bug", "feature", "refactor", "review", "chore", "docs"}
	validModes = []string{"headless", "interactive"}

	// domainTags are the controlled-vocabulary domain tags. Tags outside
	// this set and the size/type sets are rejected.
	domainTags = []string{"backend", "frontend", "infra", "docs", "ci", "auth", "db", "test"}

	// escapeHatchTags are workflow-routing opt-outs accepted by NormalizeTags
	// (so they aren't stripped) and preserved through triage (see Apply) so a
	// manually-set opt-out is never dropped. `noplan` skips the plan pipeline
	// (and, for work tasks, the human plan-review gate) and is also
	// classifier-emittable — the triage prompt instructs the model to assign it
	// for trivially mechanical small tasks, bounded by the deterministic floor
	// in ValidateVerdict (small + non-feature). `nocritic` skips the plan
	// critique and remains human/orchestrator-set only. `trivial` skips both
	// the review and testing phases (see simple-task-review.yaml and
	// testing-task.yaml) for the same trivially-mechanical class of task as
	// noplan; it is classifier-emittable and bounded by the same floor, plus a
	// stricter carve-out excluding type=bug (see ValidateVerdict) since
	// trivial has a materially larger blast radius than noplan — a false
	// positive skips the only two verification gates (review + test) before a
	// PR opens, and a "small" bug fix is exactly the case where a subtle
	// regression is likely to slip past both. `skip-testing` skips only the
	// testing workflow and preserves the prior review gate; it is accepted and
	// preserved only when set by a human/orchestrator, and ValidateVerdict
	// strips classifier-emitted instances. `notest` is deliberately not in
	// this list — it only downgrades evidence requirements (app-start
	// exemption); it never skips the tester. `notumbrella` opts a task out of
	// the ☂️-title umbrella guard (see Apply) for the rare genuine case where a
	// task legitimately keeps task_type=normal despite an umbrella-shaped title
	// or "umbrella" tag. It is accepted/preserved when already present on the
	// task, but the classifier prompt never emits it.
	escapeHatchTags = []string{"noplan", "nocritic", "trivial", "skip-testing", "notumbrella"}

	// tagAliases normalize common abbreviations into the canonical tag.
	tagAliases = map[string]string{
		"be":        "backend",
		"fe":        "frontend",
		"ops":       "infra",
		"devops":    "infra",
		"database":  "db",
		"tests":     "test",
		"testing":   "test",
		"docs:":     "docs",
		"doc":       "docs",
		"cicd":      "ci",
		"pipeline":  "ci",
		"feat":      "feature",
		"bugfix":    "bug",
		"reviewing": "review",
	}
)

// NormalizeTags validates and canonicalizes tags.
// Unknown tags are dropped with a warning in the returned error (nil if all ok).
// Duplicates are removed. Order is preserved for the first occurrence.
func NormalizeTags(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	var dropped []string
	for _, t := range raw {
		t = strings.TrimSpace(strings.ToLower(t))
		if t == "" {
			continue
		}
		if canon, ok := tagAliases[t]; ok {
			t = canon
		}
		if !isKnownTag(t) {
			dropped = append(dropped, t)
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(dropped) > 0 {
		return out, fmt.Errorf("dropped unknown tags: %v", dropped)
	}
	return out, nil
}

func isKnownTag(t string) bool {
	return slices.Contains(domainTags, t) ||
		slices.Contains(validSizes, t) ||
		slices.Contains(validTypes, t) ||
		slices.Contains(escapeHatchTags, t)
}

// ValidateVerdict ensures all enumerated fields are in their allowed set and
// that Title is non-empty. Tags are normalized in place. Mutates v.
func ValidateVerdict(v *Verdict) error {
	if strings.TrimSpace(v.Title) == "" {
		return fmt.Errorf("empty title")
	}
	if !slices.Contains(validSizes, v.Size) {
		return fmt.Errorf("invalid size %q (want small|medium|large)", v.Size)
	}
	if !slices.Contains(validTypes, v.Type) {
		return fmt.Errorf("invalid type %q (want %v)", v.Type, validTypes)
	}
	if !slices.Contains(validModes, v.Mode) {
		return fmt.Errorf("invalid mode %q (want %v)", v.Mode, validModes)
	}
	// Tag normalization: drop unknown, keep known. Errors become warnings;
	// the caller can log them but the verdict is still usable.
	norm, _ := NormalizeTags(v.Tags)
	v.Tags = norm

	// skip-testing is a manual/orchestrator escape hatch, not classifier
	// output. Existing task tags are preserved in Apply; verdict tags are not.
	v.Tags = slices.DeleteFunc(v.Tags, func(tag string) bool { return tag == "skip-testing" })

	// Deterministic floor on classifier-emitted noplan. The prompt tells the
	// model to add noplan only for small, non-feature work, but the model is
	// the one category it's told to avoid — so a code-level guard, not trust,
	// decides. noplan skips planning AND (for work tasks) the human plan-review
	// gate, so an over-eager verdict on a large/feature task would route it
	// straight to one-shot implementation unreviewed. Strip it unless the size
	// and type actually qualify. Human/orchestrator-set noplan is unaffected:
	// it lives on the task, not the verdict, and is preserved in Apply.
	if v.Size != "small" || v.Type == "feature" {
		v.Tags = slices.DeleteFunc(v.Tags, func(tag string) bool { return tag == "noplan" || tag == "trivial" })
	}
	// trivial carries a stricter floor than noplan: it also skips review and
	// testing, so a "small" bug fix (the case most likely to hide a subtle
	// regression past a self-review-skipping classifier and a no-op test
	// phase) must not qualify, even though it's allowed for noplan alone.
	if v.Type == "bug" {
		v.Tags = slices.DeleteFunc(v.Tags, func(tag string) bool { return tag == "trivial" })
	}
	return nil
}
