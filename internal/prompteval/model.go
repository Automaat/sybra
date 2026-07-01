// Package prompteval evaluates candidate prompt/skill variants offline
// (outside any live agent run) and stores durable, machine-readable verdicts
// that gate their enrollment into online A/B tests.
package prompteval

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// CandidateVariant is a prompt/skill variant to be evaluated offline before
// online A/B enrollment. Digest is the identity key: two variants with the
// same resolved prompt bytes share a verdict, and re-running after an edit
// invalidates the stale one automatically.
type CandidateVariant struct {
	ID              string `json:"id"`
	Digest          string `json:"digest"`
	Prompt          string `json:"prompt"` // resolved prompt or skill body bytes
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// Assertion is one check run against a variant's output for a golden case.
// Deterministic types (contains/not-contains/regex/is-json/latency) affect
// Score; "model-graded" is advisory-only and never fails a run by itself.
type Assertion struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// AssertionResult is the observed outcome of one Assertion.
type AssertionResult struct {
	Type   string `json:"type"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Spec is one offline evaluation unit: a candidate variant run against a
// single golden-case input and scored against its assertions.
type Spec struct {
	CaseID     string           `json:"caseId"`
	Variant    CandidateVariant `json:"variant"`
	Input      string           `json:"input"`
	Assertions []Assertion      `json:"assertions"`
	Timeout    time.Duration    `json:"timeout,omitempty"`
}

// Result is the raw, per-Spec output produced by a runner. Score is the
// fraction (0..1) of deterministic assertions that passed; a runner returns
// an error instead of a zero-value Result when the run itself could not be
// measured, so a caller never mistakes "could not run" for "scored zero".
type Result struct {
	Output     string            `json:"output"`
	Assertions []AssertionResult `json:"assertions,omitempty"`
	Score      float64           `json:"score"`
	CostUSD    float64           `json:"costUsd"`
	LatencyMS  int64             `json:"latencyMs"`
}

// Status is the tri-state offline-eval verdict.
type Status string

const (
	StatusPass        Status = "pass"
	StatusFail        Status = "fail"
	StatusUnavailable Status = "unavailable"
)

// VariantVerdict is the durable, machine-readable verdict for one
// (VariantID, Digest) pair, persisted by Store and read by Gate.
type VariantVerdict struct {
	VariantID  string            `json:"variantId"`
	Digest     string            `json:"digest"`
	Status     Status            `json:"status"`
	Score      float64           `json:"score"`
	CostUSD    float64           `json:"costUsd"`
	LatencyMS  int64             `json:"latencyMs"`
	Assertions []AssertionResult `json:"assertions,omitempty"`
	Runner     string            `json:"runner"`
	Reason     string            `json:"reason,omitempty"`
	CreatedAt  time.Time         `json:"createdAt"`
}

// Digest returns the hex sha256 of resolved prompt/skill bytes — the
// identity key used to key stored verdicts and to detect a stale prompt.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resolvedPrompt composes the candidate variant's own prompt/skill body with
// the golden case's input so a runner actually screens the digested bytes —
// not just the fixture input — against the provider. A variant with no
// resolved Prompt (e.g. a bare model/provider comparison) falls back to the
// input alone, unchanged from prior behavior.
func resolvedPrompt(spec Spec) string {
	if spec.Variant.Prompt == "" {
		return spec.Input
	}
	return spec.Variant.Prompt + "\n\n" + spec.Input
}
