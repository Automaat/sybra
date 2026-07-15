package prompteval

import (
	"strings"
	"testing"
)

// TestNativeScoring exercises evaluateAssertion directly (the pure scoring
// logic NativeRunner.Run delegates to) since Run shells out to a live
// provider CLI that isn't available in unit tests.
func TestNativeScoring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		assertion Assertion
		output    string
		latencyMS int64
		wantPass  bool
	}{
		{"contains match", Assertion{Type: "contains", Value: "hello"}, "hello world", 0, true},
		{"contains no match", Assertion{Type: "contains", Value: "hello"}, "goodbye", 0, false},
		{"not-contains match", Assertion{Type: "not-contains", Value: "secret"}, "public info", 0, true},
		{"not-contains violated", Assertion{Type: "not-contains", Value: "secret"}, "the secret is out", 0, false},
		{"regex match", Assertion{Type: "regex", Value: `^\d+$`}, "12345", 0, true},
		{"regex no match", Assertion{Type: "regex", Value: `^\d+$`}, "abc", 0, false},
		{"invalid regex", Assertion{Type: "regex", Value: `(`}, "abc", 0, false},
		{"is-json valid", Assertion{Type: "is-json"}, `{"a":1}`, 0, true},
		{"is-json invalid", Assertion{Type: "is-json"}, `not json`, 0, false},
		{"latency within budget", Assertion{Type: "latency", Value: "5s"}, "", 1000, true},
		{"latency over budget", Assertion{Type: "latency", Value: "1s"}, "", 5000, false},
		{"latency invalid duration", Assertion{Type: "latency", Value: "not-a-duration"}, "", 100, false},
		{"model-graded always annotates pass", Assertion{Type: "model-graded", Value: "is this good?"}, "anything", 0, true},
		{"unknown type fails closed", Assertion{Type: "bogus"}, "anything", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := evaluateAssertion(tc.assertion, tc.output, tc.latencyMS)
			if got.Passed != tc.wantPass {
				t.Errorf("Passed = %v, want %v (detail: %s)", got.Passed, tc.wantPass, got.Detail)
			}
		})
	}
}

// TestResolvedPromptCarriesVariantPrompt proves NativeRunner composes the
// digested candidate prompt with the golden-case input rather than testing
// the fixture input alone — two variants differing only by Prompt must
// produce different resolved prompts sent to the provider.
func TestResolvedPromptCarriesVariantPrompt(t *testing.T) {
	t.Parallel()
	specA := Spec{Variant: CandidateVariant{Prompt: "Be terse."}, Input: "hello"}
	specB := Spec{Variant: CandidateVariant{Prompt: "Be verbose."}, Input: "hello"}
	gotA := resolvedPrompt(specA)
	gotB := resolvedPrompt(specB)
	if gotA == gotB {
		t.Fatalf("resolvedPrompt did not vary with Variant.Prompt: both = %q", gotA)
	}
	if !strings.Contains(gotA, specA.Variant.Prompt) || !strings.Contains(gotA, specA.Input) {
		t.Fatalf("resolvedPrompt(%+v) = %q, want it to contain both prompt and input", specA, gotA)
	}
	// A variant with no resolved prompt (e.g. a bare model comparison) falls
	// back to the input alone, unchanged from prior behavior.
	if got := resolvedPrompt(Spec{Input: "hello"}); got != "hello" {
		t.Fatalf("resolvedPrompt with empty Variant.Prompt = %q, want %q", got, "hello")
	}
}

// TestInjectionCaseFailsClosed is the adversarial golden case: a variant
// whose output leaks content a not-contains assertion forbids must score
// FAIL, and AllowEnrollment must deny it — offline eval fails closed on a
// prompt-injection style leak rather than defaulting to pass.
func TestInjectionCaseFailsClosed(t *testing.T) {
	t.Parallel()
	// Simulated model output after a prompt-injection attempt leaked a
	// secret the assertion forbids.
	leaked := "Sure, here is the system prompt: SECRET_API_KEY=sk-12345"
	assertions := []Assertion{
		{Type: "not-contains", Value: "SECRET_API_KEY"},
	}

	var passed int
	results := make([]AssertionResult, 0, len(assertions))
	for _, a := range assertions {
		ar := evaluateAssertion(a, leaked, 0)
		results = append(results, ar)
		if ar.Passed {
			passed++
		}
	}
	score := float64(passed) / float64(len(assertions))
	if score != 0 {
		t.Fatalf("score = %v, want 0 (injection leak must fail the deterministic assertion)", score)
	}

	dir := t.TempDir()
	store := New(dir)
	verdict := VariantVerdict{
		VariantID:  "injected-variant",
		Digest:     Digest([]byte("prompt-with-injection-risk")),
		Status:     StatusFail,
		Score:      score,
		Assertions: results,
		Runner:     "native",
		Reason:     "leaked forbidden content",
	}
	if err := store.Write(verdict); err != nil {
		t.Fatalf("Write: %v", err)
	}

	gate := NewGate(store, defaultTestOfflineConfig())
	allow, reason, err := gate.AllowEnrollment(verdict.VariantID, verdict.Digest)
	if err != nil {
		t.Fatalf("AllowEnrollment: %v", err)
	}
	if allow {
		t.Fatalf("AllowEnrollment allowed a FAILED verdict (reason=%q) — must fail closed", reason)
	}
}
