package prcontent

import (
	"strings"
	"testing"
)

func TestValidateContent(t *testing.T) {
	tests := []struct {
		name    string
		content Content
		wantErr bool
	}{
		{
			name:    "valid",
			content: Content{Title: "feat(api): add endpoint", Body: "## Motivation\n\nwhy\n\n## Implementation information\n\nwhat"},
			wantErr: false,
		},
		{
			name:    "empty title",
			content: Content{Title: "  ", Body: "## Motivation\n\n## Implementation information\n"},
			wantErr: true,
		},
		{
			name:    "missing motivation",
			content: Content{Title: "fix(x): y", Body: "## Implementation information\nwhat"},
			wantErr: true,
		},
		{
			name:    "missing implementation",
			content: Content{Title: "fix(x): y", Body: "## Motivation\nwhy"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContent(&tt.content)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateContent() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildPromptIncludesCommits(t *testing.T) {
	prompt := buildPrompt(Request{
		TaskTitle:      "fix(api): handle nil",
		TaskBody:       "body text",
		CommitSubjects: []string{"fix(api): guard nil ptr", "test(api): add regression test"},
	})
	if want := "fix(api): guard nil ptr"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt missing commit subject %q:\n%s", want, prompt)
	}
	if want := "## Motivation"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt missing schema instructions %q:\n%s", want, prompt)
	}
}
