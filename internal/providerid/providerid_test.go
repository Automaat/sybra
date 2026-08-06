package providerid

import (
	"slices"
	"testing"
)

func TestAllReturnsFailoverOrderedCopy(t *testing.T) {
	got := All()
	want := []string{"claude", "codex", "copilot", "opencode"}
	if !slices.Equal(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if All()[0] != "claude" {
		t.Fatal("All() must return a defensive copy; internal slice was mutated")
	}
}

func TestIsKnown(t *testing.T) {
	for _, p := range []string{"claude", "codex", "copilot", "opencode"} {
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

// All() and IsKnown() above already pin the constants by comparing derived
// output against the literals. List() is the remaining wire surface: it feeds
// user-facing validation messages, and its separator is parsed by nothing, so
// only a test keeps it from drifting.
func TestListJoinsTheUniverseForHumans(t *testing.T) {
	if got, want := List(), "claude, codex, copilot, opencode"; got != want {
		t.Errorf("List() = %q, want %q", got, want)
	}
}
