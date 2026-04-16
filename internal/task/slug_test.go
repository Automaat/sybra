package task

import (
	"strings"
	"testing"
)

func TestSlugify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		title string
		want  string
	}{
		{"Implement auth middleware", "implement-auth-middleware"},
		{"Fix bug #42 (urgent!)", "fix-bug-42-urgent"},
		{"refactor", "refactor"},
		{"--hello world--", "hello-world"},
		{"", "task"},
		{"   ", "task"},
		{"!!!@@@", "task"},
		{"Deploy to production 🚀", "deploy-to-production"},
		{
			"This is a very long task title that exceeds the maximum allowed slug length",
			"this-is-a-very-long-task-title-that",
		},
		{"a-b", "a-b"},
		{"UPPER case MIX", "upper-case-mix"},
		{"multiple   spaces   here", "multiple-spaces-here"},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			t.Parallel()
			got := Slugify(tt.title)
			if got != tt.want {
				t.Errorf("Slugify(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}

// TestSlugifyNeverProducesUnsafeChars locks down the output alphabet: slugs
// are embedded in git branch names (sybra/<slug>-<id>) and worktree
// directory names, so any path separator, `..`, or shell metacharacter
// would corrupt branches, break git checkouts, or open injection surfaces.
// A regression that widened nonAlnum (e.g. allowed '/' for hierarchical
// slugs) would silently produce broken branch refs.
func TestSlugifyNeverProducesUnsafeChars(t *testing.T) {
	t.Parallel()
	hostile := []string{
		"../../etc/passwd",
		"../../../..",
		"a/b/c",
		`a\b\c`,
		"title with; rm -rf",
		"title with `backticks`",
		"title with $(subshell)",
		"title\nwith\nnewlines",
		"title with \x00 null",
		"😀🚀🔥 emoji-heavy",
		"\u200b\u200bzero-width\u200b",
		"   ",
		"..",
		".",
		"/",
	}
	unsafeChars := []string{"/", `\`, "..", ";", "`", "$", "\n", "\x00", " "}
	for _, h := range hostile {
		t.Run(h, func(t *testing.T) {
			t.Parallel()
			got := Slugify(h)
			if got == "" {
				t.Fatal("Slugify returned empty string; should fall back to 'task'")
			}
			for _, bad := range unsafeChars {
				if strings.Contains(got, bad) {
					t.Errorf("Slugify(%q) = %q contains unsafe char %q", h, got, bad)
				}
			}
			// Leading/trailing dashes would break `<slug>-<id>` concatenation.
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Errorf("Slugify(%q) = %q has dash at boundary — breaks slug-id joining", h, got)
			}
		})
	}
}
