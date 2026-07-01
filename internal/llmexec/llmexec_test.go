package llmexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunJSONFallsBackOnRateLimit(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}
	if res.Text != `{"ok":true}` {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestRunJSONPassesCodexPromptOnStdin(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/bash
if [ "${@: -1}" != "-" ]; then
  echo "prompt should be read from stdin" >&2
  exit 7
fi
input=$(/bin/cat)
if [ "$input" != "classify" ]; then
  echo "unexpected stdin: $input" >&2
  exit 7
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}
}

func TestRunJSONPassesCodexModel(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/bash
if [[ "$*" != *"--model gpt-5.4-mini"* ]]; then
  echo "missing model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "codex",
		Models:   map[string]string{"codex": "gpt-5.4-mini"},
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}
}

func TestRunJSONPassesCopilotModel(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "copilot"), `#!/bin/bash
if [[ "$*" != *"--model gpt-5-mini"* ]]; then
  echo "missing model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "copilot",
		Models:   map[string]string{"copilot": "gpt-5-mini"},
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "copilot" {
		t.Fatalf("provider = %q, want copilot", res.Provider)
	}
}

func TestRunJSONFallsBackOnCodexUsageLimit(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
printf '%s\n' '{"type":"error","message":"You'\''ve hit your usage limit. Try again later."}'
exit 1
`)
	writeExe(t, filepath.Join(dir, "copilot"), `#!/bin/sh
printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "copilot" {
		t.Fatalf("provider = %q, want copilot", res.Provider)
	}
	if res.Text != `{"ok":true}` {
		t.Fatalf("text = %q", res.Text)
	}
}

func TestParseCodexTextAllowsLargeStreamLine(t *testing.T) {
	text := strings.Repeat("x", 2*1024*1024)
	raw := fmt.Appendf(nil, `{"type":"item.completed","item":{"type":"agent_message","text":"%s"}}`, text)

	got, err := parseCodexText(raw)
	if err != nil {
		t.Fatalf("parseCodexText: %v", err)
	}
	if got != text {
		t.Fatalf("text length = %d, want %d", len(got), len(text))
	}
}

func TestParseCopilotTextAllowsLargeStreamLine(t *testing.T) {
	text := strings.Repeat("x", 2*1024*1024)
	raw := fmt.Appendf(nil, `{"type":"assistant.message","data":{"content":"%s"}}`, text)

	got, err := parseCopilotText(raw)
	if err != nil {
		t.Fatalf("parseCopilotText: %v", err)
	}
	if got != text {
		t.Fatalf("text length = %d, want %d", len(got), len(text))
	}
}

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
