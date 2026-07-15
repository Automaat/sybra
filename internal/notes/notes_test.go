package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSeedPrompt_NoFile_ReturnsUnchanged(t *testing.T) {
	got := SeedPrompt("do the thing", t.TempDir())
	if got != "do the thing" {
		t.Errorf("SeedPrompt with no NOTES.md = %q, want unchanged prompt", got)
	}
}

func TestSeedPrompt_EmptyDir_ReturnsUnchanged(t *testing.T) {
	if got := SeedPrompt("p", ""); got != "p" {
		t.Errorf("SeedPrompt with empty dir = %q, want %q", got, "p")
	}
}

func TestSeedPrompt_InlinesContentAndInstruction(t *testing.T) {
	dir := t.TempDir()
	body := "## Plan\n- did the migration\n## Decisions\n- chose option B"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := SeedPrompt("# Task: ship it", dir)

	if !strings.HasPrefix(got, "# Task: ship it") {
		t.Errorf("seeded prompt must preserve the original prompt at the front:\n%s", got)
	}
	for _, want := range []string{FileName, "Read it before acting", "chose option B", "did the migration"} {
		if !strings.Contains(got, want) {
			t.Errorf("seeded prompt missing %q. got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "older lines truncated") {
		t.Errorf("small NOTES.md should not be marked truncated. got:\n%s", got)
	}
}

func TestSeedPrompt_TruncatesKeepingHeadAndTail(t *testing.T) {
	dir := t.TempDir()
	headMarker := "PLAN-AND-DECISIONS-MARKER"
	tailMarker := "RECENT-DECISION-MARKER"
	body := headMarker + strings.Repeat("\nfiller line\n", seedMaxBytes) + tailMarker
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := SeedPrompt("p", dir)

	if !strings.Contains(got, "head+tail") {
		t.Errorf("oversized NOTES.md should be marked truncated. got tail:\n%s", got[max(0, len(got)-200):])
	}
	if !strings.Contains(got, headMarker) {
		t.Error("truncation must keep the structured head (Plan/Decisions)")
	}
	if !strings.Contains(got, tailMarker) {
		t.Error("truncation must keep the most recent tail")
	}
	if !strings.Contains(got, "elided") {
		t.Error("truncated seed should carry an elision marker between head and tail")
	}
	// The seed must stay bounded near the cap (head + tail + marker + framing).
	if len(got) > seedMaxBytes+2048 {
		t.Errorf("seeded prompt = %d bytes, expected bounded near seedMaxBytes=%d", len(got), seedMaxBytes)
	}
}

func TestSeedPrompt_TruncationIsRuneSafe(t *testing.T) {
	dir := t.TempDir()
	// A body of multibyte runes guarantees both the head and tail cut points
	// land mid-rune unless trimmed to a boundary.
	body := strings.Repeat("€", seedMaxBytes) // 3 bytes each, far over the cap
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := SeedPrompt("p", dir)

	if !utf8.ValidString(got) {
		t.Error("seeded prompt must be valid UTF-8 after truncation (no mid-rune cut)")
	}
	if strings.Contains(got, "�") {
		t.Error("truncation must not introduce replacement runes")
	}
}

func TestSeedPrompt_EmptyFile_PromptsToFill(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := SeedPrompt("p", dir)
	if !strings.Contains(got, "start filling it in") {
		t.Errorf("empty NOTES.md should invite the agent to fill it. got:\n%s", got)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Read(dir); ok {
		t.Error("Read should report absent file as not-ok")
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	content, ok := Read(dir)
	if !ok || content != "hi" {
		t.Errorf("Read = (%q, %v), want (%q, true)", content, ok, "hi")
	}
}
