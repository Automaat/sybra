package providerid

import (
	"slices"
	"testing"
)

func TestAllReturnsFailoverOrderedCopy(t *testing.T) {
	got := All()
	want := []string{"claude", "codex", "copilot"}
	if !slices.Equal(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if All()[0] != "claude" {
		t.Fatal("All() must return a defensive copy; internal slice was mutated")
	}
}

func TestIsKnown(t *testing.T) {
	for _, p := range []string{"claude", "codex", "copilot"} {
		if !IsKnown(p) {
			t.Errorf("IsKnown(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "gpt", "gemini", "Claude"} {
		if IsKnown(p) {
			t.Errorf("IsKnown(%q) = true, want false", p)
		}
	}
}
