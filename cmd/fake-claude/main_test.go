package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTaskID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "found after -p",
			args: []string{"claude", "-p", "work on task a1b2c3d4 now"},
			want: "a1b2c3d4",
		},
		{
			name: "no -p flag",
			args: []string{"claude", "--output-format", "stream-json"},
			want: "",
		},
		{
			name: "-p flag at end",
			args: []string{"claude", "-p"},
			want: "",
		},
		{
			name: "no hex ID in prompt",
			args: []string{"claude", "-p", "do some work please"},
			want: "",
		},
		{
			name: "empty args",
			args: []string{},
			want: "",
		},
		{
			name: "ID too short",
			args: []string{"claude", "-p", "task a1b2c3 here"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTaskID(tt.args)
			if got != tt.want {
				t.Errorf("extractTaskID(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestPopScenario_EnvVar(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO", "fail_exit")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "")

	got := popScenario()
	if got != "fail_exit" {
		t.Errorf("popScenario() = %q, want %q", got, "fail_exit")
	}
}

func TestPopScenario_Default(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "")

	got := popScenario()
	if got != "success" {
		t.Errorf("popScenario() = %q, want %q", got, "success")
	}
}

func TestPopScenario_File(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "scenarios.txt")
	if err := os.WriteFile(f, []byte("triage\nimplement\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", f)
	t.Setenv("FAKE_CLAUDE_SCENARIO", "")

	got := popScenario()
	if got != "triage" {
		t.Errorf("first pop = %q, want %q", got, "triage")
	}

	// Second pop should return the next scenario.
	got2 := popScenario()
	if got2 != "implement" {
		t.Errorf("second pop = %q, want %q", got2, "implement")
	}
}

func TestPopScenario_FileMissingFallsBack(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", "/nonexistent/path/scenarios.txt")
	t.Setenv("FAKE_CLAUDE_SCENARIO", "evaluate")

	got := popScenario()
	if got != "evaluate" {
		t.Errorf("popScenario() = %q, want %q", got, "evaluate")
	}
}
