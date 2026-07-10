package triage

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestBuildPromptInstructsNoplan(t *testing.T) {
	p := buildPrompt(task.Task{Title: "fix CI on renovate PR", Body: ""}, nil)
	for _, want := range []string{"noplan", "Decision guide for noplan"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q; classifier won't be told to emit noplan", want)
		}
	}
}

func TestFallbackClassifierPassesConfiguredClaudeModel(t *testing.T) {
	dir := t.TempDir()
	writeTestExe(t, filepath.Join(dir, "claude"), `#!/bin/bash
if [[ "$*" != *"--model opus"* ]]; then
  echo "missing configured model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"result":"{\"title\":\"fix(api): handle bug\",\"original_title\":\"bug\",\"description\":\"\",\"tags\":[\"backend\",\"small\",\"bug\"],\"size\":\"small\",\"type\":\"bug\",\"mode\":\"headless\",\"project_id\":\"\"}"}'
`)
	t.Setenv("PATH", dir)

	_, err := (&FallbackClassifier{
		Model:  "opus",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}).Classify(context.Background(), task.Task{Title: "bug"}, nil)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
}

func TestFallbackClassifierPassesStructuredOutputSchema(t *testing.T) {
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-args.txt")
	writeTestExe(t, filepath.Join(dir, "claude"), `#!/bin/bash
printf '%s' "$*" > `+captured+`
printf '%s\n' '{"result":"{\"title\":\"fix(api): handle bug\",\"original_title\":\"bug\",\"description\":\"\",\"tags\":[\"backend\",\"small\",\"bug\"],\"size\":\"small\",\"type\":\"bug\",\"mode\":\"headless\",\"project_id\":\"\"}"}'
`)
	t.Setenv("PATH", dir)

	_, err := (&FallbackClassifier{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}).Classify(context.Background(), task.Task{Title: "bug"}, nil)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(got), `"additionalProperties":false`) {
		t.Errorf("claude invocation did not carry the triage.Schema structured-output schema: %s", got)
	}
}

func writeTestExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
