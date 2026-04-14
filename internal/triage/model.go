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

var (
	validSizes = []string{"small", "medium", "large"}
	validTypes = []string{"bug", "feature", "refactor", "review", "chore", "docs"}
	validModes = []string{"headless", "interactive"}

	// domainTags are the controlled-vocabulary domain tags. Tags outside
	// this set and the size/type sets are rejected.
	domainTags = []string{"backend", "frontend", "infra", "docs", "ci", "auth", "db", "test"}

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
		slices.Contains(validTypes, t)
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
	return nil
}
