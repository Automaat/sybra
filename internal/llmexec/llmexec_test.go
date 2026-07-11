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

func TestRunJSONPassesCodexOutputSchemaAsTempFile(t *testing.T) {
	dir := t.TempDir()
	captureDir := t.TempDir()
	writeExe(t, filepath.Join(dir, "codex"), fmt.Sprintf(`#!/bin/bash
prev=""
for arg in "$@"; do
  if [ "$prev" = "--output-schema" ]; then
    printf '%%s' "$arg" > %q/schema-arg.txt
    printf '%%s' "$(< "$arg")" > %q/schema-path.txt
  fi
  prev="$arg"
done
printf '%%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`, captureDir, captureDir))
	t.Setenv("PATH", dir)

	const schema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	res, err := RunJSON(context.Background(), "classify", Options{Provider: "codex", Schema: schema})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}

	schemaPathRaw, err := os.ReadFile(filepath.Join(captureDir, "schema-arg.txt"))
	if err != nil {
		t.Fatalf("read captured schema arg: %v", err)
	}
	if strings.TrimSpace(string(schemaPathRaw)) == "" {
		t.Fatalf("--output-schema flag was not passed to codex")
	}

	gotSchema, err := os.ReadFile(filepath.Join(captureDir, "schema-path.txt"))
	if err != nil {
		t.Fatalf("read schema temp file: %v", err)
	}
	if string(gotSchema) != schema {
		t.Fatalf("schema temp file content = %q, want %q", gotSchema, schema)
	}
}

func TestRunJSONEmbedsSchemaAsProseForClaude(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/bash
if [[ "$*" == *"--output-schema"* ]]; then
  echo "claude must not receive --output-schema" >&2
  exit 7
fi
if [[ "$*" != *"Output schema:"* ]]; then
  echo "prompt missing schema prose: $*" >&2
  exit 7
fi
printf '%s\n' '{"result":"{\"ok\":true}"}'
`)
	t.Setenv("PATH", dir)

	const schema = `{"type":"object","properties":{"ok":{"type":"boolean"}}}`
	res, err := RunJSON(context.Background(), "classify", Options{Provider: "claude", Schema: schema})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "claude" {
		t.Fatalf("provider = %q, want claude", res.Provider)
	}
}

func TestRunJSONEmptySchemaAddsNoFlagOrProse(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/bash
if [[ "$*" == *"--output-schema"* ]]; then
  echo "unexpected --output-schema with empty schema" >&2
  exit 7
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{Provider: "codex"})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", res.Provider)
	}
}

func TestRunJSONFallsBackWhenSchemaTempFileFails(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
echo "codex should never run when schema delivery fails" >&2
exit 7
`)
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
printf '%s\n' '{"result":"{\"ok\":true}"}'
`)
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", filepath.Join(dir, "does-not-exist"))

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "codex",
		Schema:   `{"type":"object"}`,
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "claude" {
		t.Fatalf("provider = %q, want claude fallback after schema delivery failure", res.Provider)
	}
}

func TestRunJSONFallsBackWhenCodexRejectsOutputSchemaFlag(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
echo "error: unexpected argument '--output-schema' found" >&2
exit 2
`)
	writeExe(t, filepath.Join(dir, "copilot"), `#!/bin/sh
printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "codex",
		Schema:   `{"type":"object"}`,
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "copilot" {
		t.Fatalf("provider = %q, want copilot fallback after codex rejects --output-schema", res.Provider)
	}
}

func TestRunJSONAllProvidersFailedNamesLastProvider(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "claude"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "codex"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	writeExe(t, filepath.Join(dir, "copilot"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{})
	if err == nil {
		t.Fatal("RunJSON: want error when every provider fails")
	}
	if res.Provider != "copilot" {
		t.Fatalf("provider = %q, want copilot (last candidate tried)", res.Provider)
	}
}

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
