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
		t.Errorf("regular (non-OneShot) interactive runs persist across turns and should not get the one-shot-only guardrail; got:\n%s", got)
	}
}

func TestWithBackgroundTaskGuardrail_InteractiveOneShot(t *testing.T) {
	// A OneShot interactive dispatch is a single detached conversational turn
	// (RunConfig.OneShot's doc comment) — it exits after the first result
	// event exactly like headless, so it shares the same silent-corruption
	// failure mode and must get the same guardrail. See task e150a89b's
	// lost_agent incident (monitor investigation 73474d71): an interactive
	// OneShot implementation run deferred `make check` to the background and
	// ended its turn, killing the check before it could report back.
	got := withBackgroundTaskGuardrail("do the thing", RunConfig{Mode: "interactive", OneShot: true, SeedWorkingMemory: true})
	if got == "do the thing" {
		t.Fatal("expected guardrail to be appended for an interactive OneShot code-authoring run")
	}
	if !strings.Contains(got, "background") {
		t.Errorf("prompt missing background task guardrail text:\n%s", got)
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
