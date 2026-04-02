package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Task
		wantErr bool
	}{
		{
			name: "valid frontmatter with body",
			input: `---
id: abc123
title: Test task
status: todo
agent_mode: headless
tags: [backend, auth]
---
## Description
Some body content`,
			want: Task{
				ID:        "abc123",
				Title:     "Test task",
				Status:    StatusTodo,
				AgentMode: "headless",
				Tags:      []string{"backend", "auth"},
				Body:      "## Description\nSome body content",
			},
		},
		{
			name: "valid frontmatter without body",
			input: `---
id: def456
title: Empty body task
status: done
---
`,
			want: Task{
				ID:     "def456",
				Title:  "Empty body task",
				Status: StatusDone,
			},
		},
		{
			name:    "missing delimiters",
			input:   "no frontmatter here",
			wantErr: true,
		},
		{
			name:    "only one delimiter",
			input:   "---\nid: test\n",
			wantErr: true,
		},
		{
			name: "allowed_tools parsed",
			input: `---
id: t1
title: With tools
status: todo
allowed_tools: [Read, Write, Bash]
---`,
			want: Task{
				ID:           "t1",
				Title:        "With tools",
				Status:       StatusTodo,
				AllowedTools: []string{"Read", "Write", "Bash"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBytes([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.want.ID {
				t.Errorf("ID = %q, want %q", got.ID, tt.want.ID)
			}
			if got.Title != tt.want.Title {
				t.Errorf("Title = %q, want %q", got.Title, tt.want.Title)
			}
			if got.Status != tt.want.Status {
				t.Errorf("Status = %q, want %q", got.Status, tt.want.Status)
			}
			if got.AgentMode != tt.want.AgentMode {
				t.Errorf("AgentMode = %q, want %q", got.AgentMode, tt.want.AgentMode)
			}
			if got.Body != tt.want.Body {
				t.Errorf("Body = %q, want %q", got.Body, tt.want.Body)
			}
			if len(got.Tags) != len(tt.want.Tags) {
				t.Errorf("Tags = %v, want %v", got.Tags, tt.want.Tags)
			}
			if len(got.AllowedTools) != len(tt.want.AllowedTools) {
				t.Errorf("AllowedTools = %v, want %v", got.AllowedTools, tt.want.AllowedTools)
			}
		})
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	content := `---
id: file-test
title: From file
status: in-progress
---
Body here`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := Parse(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task.ID != "file-test" {
		t.Errorf("ID = %q, want %q", task.ID, "file-test")
	}
	if task.FilePath != path {
		t.Errorf("FilePath = %q, want %q", task.FilePath, path)
	}
}

func TestParseNonexistentFile(t *testing.T) {
	_, err := Parse("/nonexistent/path.md")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestMarshal(t *testing.T) {
	task := Task{
		ID:        "m1",
		Title:     "Marshal test",
		Status:    StatusTodo,
		AgentMode: "headless",
		Body:      "Some body",
	}

	data, err := Marshal(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		t.Error("missing opening delimiter")
	}
	if !strings.Contains(s, "id: m1") {
		t.Error("missing id field")
	}
	if !strings.Contains(s, "title: Marshal test") {
		t.Error("missing title field")
	}
	if !strings.Contains(s, "status: todo") {
		t.Error("missing status field")
	}
	if !strings.HasSuffix(s, "Some body\n") {
		t.Errorf("unexpected body suffix: %q", s[len(s)-20:])
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	original := Task{
		ID:        "rt1",
		Title:     "Round trip",
		Status:    StatusBlocked,
		AgentMode: "interactive",
		Tags:      []string{"test", "ci"},
		Body:      "## Steps\n- Step one\n- Step two",
	}

	data, err := Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.ID != original.ID {
		t.Errorf("ID = %q, want %q", parsed.ID, original.ID)
	}
	if parsed.Title != original.Title {
		t.Errorf("Title = %q, want %q", parsed.Title, original.Title)
	}
	if parsed.Status != original.Status {
		t.Errorf("Status = %q, want %q", parsed.Status, original.Status)
	}
	if parsed.Body != original.Body {
		t.Errorf("Body = %q, want %q", parsed.Body, original.Body)
	}
	if len(parsed.Tags) != len(original.Tags) {
		t.Errorf("Tags = %v, want %v", parsed.Tags, original.Tags)
	}
}

func TestMarshalEmptyBody(t *testing.T) {
	task := Task{ID: "e1", Title: "No body", Status: StatusTodo}
	data, err := Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	// Should end with closing delimiter, no trailing body
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if lines[len(lines)-1] != "---" {
		t.Errorf("last line = %q, want %q", lines[len(lines)-1], "---")
	}
}
