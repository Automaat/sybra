package task

import "testing"

func TestSlugify(t *testing.T) {
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
			got := Slugify(tt.title)
			if got != tt.want {
				t.Errorf("Slugify(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
