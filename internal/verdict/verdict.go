// Package verdict is the single shared parser for the "human vs sybra_bug"
// verdict produced by human-review-role headless agents. It is consumed by
// the human-review handler, agent_completion, and the monitor detector so
// all three read the same normalized decision instead of maintaining their
// own regex/JSON heuristics.
package verdict

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Decision is the agent's structured verdict output.
type Decision struct {
	Decision    string   `json:"decision"` // "human" | "sybra_bug"
	Summary     string   `json:"summary"`
	IssueTitle  string   `json:"issue_title,omitempty"`
	IssueBody   string   `json:"issue_body,omitempty"`
	IssueLabels []string `json:"issue_labels,omitempty"`
}

// Source records which parse path produced a Decision, for audit trails.
type Source string

const (
	// SourceJSON means the verdict was decoded from bare structured-output
	// JSON — the result of a headless run made with --json-schema.
	SourceJSON Source = "json"
	// SourceFence means the verdict was extracted from a legacy fenced
	// ```sybra-verdict``` block, for agents/prompts predating --json-schema.
	SourceFence Source = "fence"
)

// Schema is the JSON Schema passed to --json-schema for verdict-producing
// headless roles (see agent.RunConfig.OutputSchema).
//
// Codex enforces OpenAI strict structured-output rules: with
// additionalProperties:false, every key in properties must appear in
// required. Optional fields (issue_* — populated only for sybra_bug) are
// therefore modeled as nullable and listed as required; the model emits null
// for them on a human verdict. Go decodes JSON null into the zero value
// (empty string / nil slice), so normalize() sees them as absent.
const Schema = `{"type":"object","properties":{"decision":{"type":"string","enum":["human","sybra_bug"]},"summary":{"type":"string"},"issue_title":{"type":["string","null"]},"issue_body":{"type":["string","null"]},"issue_labels":{"type":["array","null"],"items":{"type":"string"}}},"required":["decision","summary","issue_title","issue_body","issue_labels"],"additionalProperties":false}`

var fenceRe = regexp.MustCompile("(?s)```\\s*sybra-verdict\\s*\\n(.*?)\\n```")

// Parse extracts a Decision from an agent's final assistant text. It tries
// bare JSON first — the shape produced by a --json-schema structured-output
// run — decoding only the first JSON value so trailing prose does not
// invalidate the parse. If that fails to decode structurally, it falls back
// to the legacy fenced ```sybra-verdict``` block for prompts/agents that
// predate --json-schema. Fails closed (returns an error, never panics) on
// empty input, invalid/case-mismatched decisions, empty summaries,
// malformed issue_labels, and envelope/object-shaped input that does not
// carry a top-level decision.
func Parse(text string) (Decision, Source, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Decision{}, "", errors.New("verdict: empty input")
	}
	if v, err := decodeFirstJSON(trimmed); err == nil {
		return normalize(v, SourceJSON)
	}
	if v, err := decodeFenced(text); err == nil {
		return normalize(v, SourceFence)
	}
	return Decision{}, "", errors.New("verdict: no parseable decision found")
}

// decodeFirstJSON decodes only the first JSON value in text, ignoring any
// trailing content. This gives bare JSON precedence when both a bare
// structured-output object and a legacy fenced block are present, and
// tolerates a model that appends trailing prose after the JSON.
func decodeFirstJSON(text string) (Decision, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	var v Decision
	if err := dec.Decode(&v); err != nil {
		return Decision{}, fmt.Errorf("verdict: bare json: %w", err)
	}
	return v, nil
}

func decodeFenced(text string) (Decision, error) {
	m := fenceRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return Decision{}, errors.New("verdict: no sybra-verdict block")
	}
	var v Decision
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v); err != nil {
		return Decision{}, fmt.Errorf("verdict: fenced json: %w", err)
	}
	return v, nil
}

func normalize(v Decision, src Source) (Decision, Source, error) {
	v.Decision = strings.ToLower(strings.TrimSpace(v.Decision))
	v.Summary = strings.TrimSpace(v.Summary)
	v.IssueTitle = strings.TrimSpace(v.IssueTitle)
	v.IssueBody = strings.TrimSpace(v.IssueBody)
	v.IssueLabels = normalizeLabels(v.IssueLabels)
	if v.Decision != "human" && v.Decision != "sybra_bug" {
		return Decision{}, "", fmt.Errorf("verdict: invalid decision %q", v.Decision)
	}
	if v.Summary == "" {
		return Decision{}, "", errors.New("verdict: empty summary")
	}
	return v, src, nil
}

// normalizeLabels trims each label and drops any that are blank, so
// downstream consumers (e.g. app_human_review.go's task tags) never see
// whitespace-only or empty labels from a model's issue_labels output.
func normalizeLabels(labels []string) []string {
	if labels == nil {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
