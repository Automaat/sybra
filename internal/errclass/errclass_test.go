package errclass

import "testing"

// TestIsBadRef pins the twelve needles that lived byte-identically in
// internal/project and internal/workflow.
func TestIsBadRef(t *testing.T) {
	for _, text := range []string{
		"fatal: bad object HEAD",
		"error: object file .git/objects/ab/cdef is empty",
		"fatal: ambiguous argument 'origin/main...HEAD': unknown revision",
		"fatal: Invalid revision range origin/main...HEAD",
	} {
		if !IsBadRef(text) {
			t.Fatalf("IsBadRef(%q) = false, want true", text)
		}
	}
	if IsBadRef("connection refused") {
		t.Fatal("IsBadRef matched a network error")
	}
	if IsBadRef("") {
		t.Fatal("IsBadRef matched empty input")
	}
}

func TestMatchesIsCaseInsensitive(t *testing.T) {
	if !matches("FATAL: BAD OBJECT HEAD", badRefPhrases) {
		t.Fatal("matches should lowercase the input")
	}
	if matches("anything", nil) {
		t.Fatal("matches with no families should be false")
	}
}
