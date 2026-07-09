package scrub

import (
	"strings"
	"testing"
)

// FuzzScrub asserts the security-critical invariant of the redactor: any
// non-empty blocklist literal that appears in the input must NOT appear in
// the output. This is the regex floor of Work-Data Confidentiality — if
// fuzzing ever finds a counterexample, the scrubber is broken and work
// identifiers could leak into public artifacts.
func FuzzScrub(f *testing.F) {
	type seed struct {
		text  string
		block string
	}
	seeds := []seed{
		{"", ""},
		{"hello world", "world"},
		{"https://github.com/owner/repo", "owner/repo"},
		{"see ABC-123 for context", ""},
		{"owner/repo and owner/repo again", "owner/repo"},
		{"contact me at user@example.com", ""},
		{"\xef\xbb\xbf owner-prefix owner", "owner"},
		{"multi line\nblock\nlist", "block"},
		{"overlap: aaaaa", "aa"},
		{"see https://github.com/work-org/secret-repo-2/pull/9 for context", ""},
		{"task from work-org here", "act"},
	}
	for _, s := range seeds {
		f.Add(s.text, s.block)
	}

	f.Fuzz(func(t *testing.T, text, block string) {
		out, _ := Scrub(text, []string{block})

		// Invariant 1: never panics (implicit — fuzzer catches panics).

		// Invariant 2: a non-empty, non-whitespace blocklist literal must
		// be fully redacted from the output. Two exclusions:
		//   - literal contains the placeholder (would chase its tail)
		//   - placeholder contains the literal as a substring, e.g. block
		//     "a" appears inside "[redacted]" — that's the redaction
		//     marker, not a leak.
		trimmed := strings.TrimSpace(block)
		if trimmed == "" || strings.Contains(trimmed, Placeholder) || strings.Contains(Placeholder, trimmed) {
			return
		}
		if strings.Contains(text, trimmed) && strings.Contains(out, trimmed) {
			t.Fatalf("scrub leaked blocklist literal %q\n  input:  %q\n  output: %q", trimmed, text, out)
		}
	})
}
