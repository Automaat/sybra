package agent

import (
	"strings"
	"testing"
)

func TestWithBackgroundTaskGuardrail_HeadlessCodeAuthor(t *testing.T) {
	got := withBackgroundTaskGuardrail("do the thing", RunConfig{Mode: "headless", SeedWorkingMemory: true})
	if got == "do the thing" {
		t.Fatal("expected guardrail to be appended for a headless, code-authoring run")
	}
	if !strings.HasPrefix(got, "do the thing") {
		t.Errorf("guardrail must be appended after the original prompt, not replace it:\n%s", got)
	}
	if !strings.Contains(got, "background") {
		t.Errorf("prompt missing background task guardrail text:\n%s", got)
	}
}

func TestWithBackgroundTaskGuardrail_SkipsInteractive(t *testing.T) {
	got := withBackgroundTaskGuardrail("do the thing", RunConfig{Mode: "interactive", SeedWorkingMemory: true})
	if got != "do the thing" {
		t.Errorf("interactive runs persist across turns and should not get the headless-only guardrail; got:\n%s", got)
	}
}

func TestWithBackgroundTaskGuardrail_SkipsVerifierRoles(t *testing.T) {
	// SeedWorkingMemory is only ever set true for code-author roles
	// (see Role.AuthorsCode); a verifier role (review/test-runner/eval) never
	// sets it, so this locks in that the guardrail follows the same gate.
	got := withBackgroundTaskGuardrail("do the thing", RunConfig{Mode: "headless", SeedWorkingMemory: false})
	if got != "do the thing" {
		t.Errorf("non-code-author headless runs should not get the guardrail; got:\n%s", got)
	}
}
