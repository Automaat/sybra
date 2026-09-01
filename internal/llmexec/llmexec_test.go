package llmexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
)

func TestRunJSONFallsBackOnRateLimit(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
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
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
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

func TestRunJSONPassesOpenCodeModel(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, "opencode"), `#!/bin/bash
if [[ "$*" != *"--model openrouter/z-ai/glm-5.2"* ]]; then
  echo "missing model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"ok\":true}"}}'
`)
	t.Setenv("PATH", dir)

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "opencode",
		Models:   map[string]string{"opencode": "openrouter/z-ai/glm-5.2"},
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.Provider != "opencode" {
		t.Fatalf("provider = %q, want opencode", res.Provider)
	}
}

func TestRunJSONFallsBackOnCodexUsageLimit(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
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

func TestParseOpenCodeTextAllowsLargeStreamLine(t *testing.T) {
	text := strings.Repeat("x", 2*1024*1024)
	raw := fmt.Appendf(nil, `{"type":"assistant.message","data":{"content":"%s"}}`, text)

	got, err := parseOpenCodeText(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeText: %v", err)
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
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/bash
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
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
printf '%s\n' '{"result":"{\"ok\":true}"}'
`)
	t.Setenv("PATH", dir)
	// Breaks the schema temp file only. Dir keeps the call's own working
	// directory off this unusable temp root, so the failover under test is
	// still schema delivery rather than a missing working directory.
	t.Setenv("TMPDIR", filepath.Join(dir, "does-not-exist"))

	res, err := RunJSON(context.Background(), "classify", Options{
		Provider: "codex",
		Schema:   `{"type":"object"}`,
		Dir:      t.TempDir(),
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
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
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
	writeExe(t, filepath.Join(dir, "opencode"), `#!/bin/sh
echo "rate limit exceeded" >&2
exit 1
`)
	t.Setenv("PATH", dir)

	// Tools on, so the chain still ends at opencode: with tools off it is not
	// a fallback, which TestCandidatesDropsToolOnlyFallback covers.
	res, err := RunJSON(context.Background(), "classify", Options{EnableTools: true})
	if err == nil {
		t.Fatal("RunJSON: want error when every provider fails")
	}
	if res.Provider != "opencode" {
		t.Fatalf("provider = %q, want opencode (last candidate tried)", res.Provider)
	}
}

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRunJSONWithNilHealthChecker is the regression test for the config
// docs/manual-testing.md recommends: providers.health_check.enabled=false.
//
// App.initProviderHealth leaves the checker nil there and callers box it into
// Options.Gate, so `opts.Gate != nil` below is TRUE for a nil *Checker and the
// IsHealthy call went straight into a nil dereference — crashing planning,
// triage, PR content, umbrella expansion, and the digest, all of which reach
// RunJSON through this line.
func TestRunJSONWithNilHealthChecker(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, providerid.Claude), `#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"success","result":"{\"ok\":true}","total_cost_usd":0.01}'
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Exactly what the disabled-health-check path hands callers: a nil
	// *provider.Checker in a non-nil provider.HealthGate interface.
	var gate provider.HealthGate = (*provider.Checker)(nil)

	res, err := RunJSON(context.Background(), "hi", Options{Provider: "claude", Gate: gate})
	if err != nil {
		t.Fatalf("RunJSON with health checks disabled: %v", err)
	}
	if res.Provider != "claude" {
		t.Errorf("Provider = %q, want claude: an absent gate must block nothing", res.Provider)
	}
	if !strings.Contains(res.Text, `"ok"`) {
		t.Errorf("Text = %q, want the provider's payload", res.Text)
	}
}

// TestRunJSONKeepsToolsOffByDefault pins the inversion in #3383. These calls
// are classifiers and judges built from GitHub-sourced text, and every one of
// them used to hand its provider a fully-permissioned session.
func TestRunJSONKeepsToolsOffByDefault(t *testing.T) {
	for _, tc := range []struct {
		provider string
		script   string
		wantArg  string
		denyArg  string
	}{
		{
			provider: providerid.Claude,
			script:   `printf '%s\n' "$@" > "$ARGS_FILE"; printf '%s\n' '{"result":"{\"ok\":true}"}'`,
			wantArg:  "--disallowedTools",
			denyArg:  "--dangerously-skip-permissions",
		},
		{
			provider: providerid.Codex,
			script:   `printf '%s\n' "$@" > "$ARGS_FILE"; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'`,
			wantArg:  "read-only",
			denyArg:  "--dangerously-bypass-approvals-and-sandbox",
		},
		{
			provider: providerid.Copilot,
			script:   `printf '%s\n' "$@" > "$ARGS_FILE"; printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"ok\":true}"}}'`,
			wantArg:  "--no-ask-user",
			denyArg:  "--allow-all-tools",
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "args")
			writeExe(t, filepath.Join(dir, tc.provider), "#!/bin/sh\nARGS_FILE="+argsFile+"\n"+tc.script+"\n")
			t.Setenv("PATH", dir)

			if _, err := RunJSON(context.Background(), "classify", Options{Provider: tc.provider}); err != nil {
				t.Fatalf("RunJSON: %v", err)
			}
			recorded, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			got := string(recorded)
			if !strings.Contains(got, tc.wantArg) {
				t.Errorf("argv lacks %q:\n%s", tc.wantArg, got)
			}
			if strings.Contains(got, tc.denyArg) {
				t.Errorf("argv still carries %q:\n%s", tc.denyArg, got)
			}
		})
	}
}

// TestRunJSONRunsOutsideTheCallersDirectory pins the other half: the CLI used
// to inherit the serving process's cwd, which on the deploy host is Sybra's
// own checkout, so a tool-enabled call could write into the source tree.
func TestRunJSONRunsOutsideTheCallersDirectory(t *testing.T) {
	dir := t.TempDir()
	cwdFile := filepath.Join(dir, "cwd")
	writeExe(t, filepath.Join(dir, providerid.Claude), "#!/bin/sh\npwd > "+cwdFile+"\nprintf '%s\\n' '{\"result\":\"{\\\"ok\\\":true}\"}'\n")
	t.Setenv("PATH", dir)

	if _, err := RunJSON(context.Background(), "classify", Options{Provider: providerid.Claude}); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	recorded, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("read recorded cwd: %v", err)
	}
	self, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if got := strings.TrimSpace(string(recorded)); got == self {
		t.Fatalf("provider ran in the caller's directory %q", got)
	}
}

// TestRunJSONUsesTheRegisteredCommandFactory pins the seam the app wires: the
// sandbox lives in internal/agent, which imports this package, so containment
// arrives by registration. A call made after registration must not spawn the
// CLI itself.
func TestRunJSONUsesTheRegisteredCommandFactory(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, filepath.Join(dir, providerid.Claude), "#!/bin/sh\nexit 9\n")
	stand := filepath.Join(dir, "stand-in")
	writeExe(t, stand, "#!/bin/sh\nprintf '%s\\n' '{\"result\":\"{\\\"ok\\\":true}\"}'\n")
	t.Setenv("PATH", dir)

	var gotDir, gotName string
	cleaned := false
	SetCommandFactory(func(ctx context.Context, d, name string, _ []string) (*exec.Cmd, func(), error) {
		gotDir, gotName = d, name
		return exec.CommandContext(ctx, stand), func() { cleaned = true }, nil
	})
	t.Cleanup(func() { SetCommandFactory(nil) })

	if _, err := RunJSON(context.Background(), "classify", Options{Provider: providerid.Claude}); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if gotName != "claude" {
		t.Errorf("factory got name %q, want claude", gotName)
	}
	if gotDir == "" {
		t.Error("factory got no working directory")
	}
	if !cleaned {
		t.Error("factory cleanup never ran; a scratch home per call would accumulate")
	}
}

// TestCandidatesDropsToolOnlyFallback pins the failover half of the tools-off
// default. opencode's non-interactive mode approves every tool call and has no
// verified alternative, so it must not be the hop a tools-off call silently
// lands on. An explicit preference still reaches it, since that caller chose
// the CLI knowingly.
func TestCandidatesDropsToolOnlyFallback(t *testing.T) {
	if got := candidates("", false); slices.Contains(got, providerid.OpenCode) {
		t.Errorf("tools-off fallback chain still contains opencode: %v", got)
	}
	if got := candidates("", true); !slices.Contains(got, providerid.OpenCode) {
		t.Errorf("tools-on chain dropped opencode: %v", got)
	}
	if got := candidates(providerid.OpenCode, false); got[0] != providerid.OpenCode {
		t.Errorf("explicit opencode preference was dropped: %v", got)
	}
}

func TestProviderError_SalvagesTheReasonFromStdout(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		stdout string
		want   string
	}{
		{
			name:   "stderr wins when present",
			stderr: "boom from stderr",
			stdout: `{"type":"error","message":"{\"error\":{\"message\":\"ignored\"}}"}`,
			want:   "boom from stderr",
		},
		{
			name:   "json error event on stdout is used when stderr is empty",
			stdout: "{\"type\":\"thread.started\"}\n{\"type\":\"error\",\"message\":\"invalid_json_schema: uniqueItems is not permitted\"}\n",
			want:   "invalid_json_schema: uniqueItems is not permitted",
		},
		{
			name:   "nested error message is used",
			stdout: `{"type":"turn.failed","error":{"message":"400 invalid schema"}}`,
			want:   "400 invalid schema",
		},
		{
			name:   "plain stdout carries no reason",
			stdout: "just some output\n",
			want:   "",
		},
		{
			name:   "string-valued error field is read",
			stdout: `{"type":"error","error":"401 unauthorized"}`,
			want:   "401 unauthorized",
		},
		{
			name:   "failure-typed event without an error field is read",
			stdout: `{"type":"thread.error","message":"real failure"}`,
			want:   "real failure",
		},
		{
			name:   "an ordinary assistant message is never reported as the cause",
			stdout: `{"type":"item.completed","level":"error","message":"assistant said: umbrella acme/private#42"}`,
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := providerError(providerid.Codex, errors.New("exit status 1"), tc.stderr, tc.stdout)
			if !strings.Contains(err.Error(), "exit status 1") {
				t.Fatalf("error lost the exit status: %v", err)
			}
			if tc.want == "" {
				if err.Error() != providerid.Codex+": exit status 1" {
					t.Fatalf("error = %q, want the bare exit status", err)
				}
				return
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to carry %q", err, tc.want)
			}
		})
	}
}

func TestProviderStdoutDetail_IsBounded(t *testing.T) {
	long := strings.Repeat("x", providerDetailMax*3)
	detail := providerStdoutDetail(`{"type":"error","message":"` + long + `"}`)
	if len(detail) > providerDetailMax+len("...") {
		t.Fatalf("detail length = %d, want at most %d", len(detail), providerDetailMax+len("..."))
	}
	if detail == "" {
		t.Fatal("bounded detail was dropped entirely")
	}
}
