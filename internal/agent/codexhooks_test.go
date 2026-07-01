package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// makeFakeSybraCLI creates a minimal executable named sybra-cli in dir and
// prepends dir to PATH so resolveCodexHookBin finds it via exec.LookPath.
func makeFakeSybraCLI(t *testing.T) string {
	t.Helper()
	return makeFakeHookBinary(t, "sybra-cli")
}

func makeFakeHookBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func makeOnlyFakeHookBinary(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestBuildCodexHookArgs_FourEvents(t *testing.T) {
	makeFakeSybraCLI(t)

	args := buildCodexHookArgs("task-abc123")
	if len(args) == 0 {
		t.Fatal("expected hook args, got none")
	}

	wantEvents := []string{"SessionStart", "SubagentStart", "SubagentStop", "Stop"}
	for _, event := range wantEvents {
		key := "hooks." + event + "="
		found := false
		for _, arg := range args {
			if strings.HasPrefix(arg, key) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("hook override for event %q not found; args=%v", event, args)
		}
	}
}

func TestBuildCodexHookArgs_KlaudiushPreToolUse(t *testing.T) {
	makeFakeHookBinary(t, "klaudiush")

	args := buildCodexHookArgs("task-abc123")
	found := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "hooks.PreToolUse=") &&
			strings.Contains(arg, "klaudiush --provider codex --event PreToolUse") &&
			!strings.Contains(arg, `run_mode=`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("klaudiush PreToolUse hook not found; args=%v", args)
	}
}

func TestBuildCodexHookArgs_BypassHookTrustPresent(t *testing.T) {
	makeFakeSybraCLI(t)

	args := buildCodexHookArgs("task-abc123")
	if !slices.Contains(args, "--dangerously-bypass-hook-trust") {
		t.Errorf("--dangerously-bypass-hook-trust missing; args=%v", args)
	}
}

func TestBuildCodexHookArgs_ArgsArePairs(t *testing.T) {
	makeFakeSybraCLI(t)

	args := buildCodexHookArgs("task-abc123")
	// Every -c must be immediately followed by its value (not another flag).
	for i, arg := range args {
		if arg == "-c" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				t.Errorf("-c at index %d not followed by a value; args=%v", i, args)
			}
		}
	}
}

func TestBuildCodexHookArgs_TaskIDInjectionRejected(t *testing.T) {
	badIDs := []string{
		"",
		"task id with spaces",
		"task\nid",
		"task;rm -rf /",
		"task$(id)",
	}
	for _, id := range badIDs {
		args := buildCodexHookArgs(id)
		if len(args) != 0 {
			t.Errorf("buildCodexHookArgs(%q) should return nil for unsafe taskID; got %v", id, args)
		}
	}
}

func TestBuildCodexHookArgs_MissingBinariesReturnsNil(t *testing.T) {
	// Point PATH to an empty dir with no sybra-cli or klaudiush.
	t.Setenv("PATH", t.TempDir())

	args := buildCodexHookArgs("task-abc123")
	if len(args) != 0 {
		t.Errorf("expected nil when hook binaries are missing; got %v", args)
	}
}

func TestBuildCodexHookArgs_KlaudiushOnlyStillHooks(t *testing.T) {
	makeOnlyFakeHookBinary(t, "klaudiush")

	args := buildCodexHookArgs("task-abc123")
	if !slices.Contains(args, "--dangerously-bypass-hook-trust") {
		t.Errorf("--dangerously-bypass-hook-trust missing; args=%v", args)
	}
	found := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "hooks.PreToolUse=") {
			found = true
		}
		if strings.HasPrefix(arg, "hooks.SessionStart=") {
			t.Errorf("sybra-cli telemetry hook should be absent without sybra-cli; args=%v", args)
		}
	}
	if !found {
		t.Errorf("klaudiush PreToolUse hook missing; args=%v", args)
	}
}

func TestBuildCodexHookArgs_CommandContainsTaskID(t *testing.T) {
	makeFakeSybraCLI(t)

	taskID := "task-xyz789"
	args := buildCodexHookArgs(taskID)

	// Each hook override value must embed the task ID.
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) {
			if strings.HasPrefix(args[i+1], "hooks.PreToolUse=") {
				continue
			}
			if !strings.Contains(args[i+1], taskID) {
				t.Errorf("-c value at index %d does not contain taskID %q; value=%q", i+1, taskID, args[i+1])
			}
		}
	}
}

func TestBuildClaudeHookSettings_KlaudiushPreToolUse(t *testing.T) {
	makeFakeHookBinary(t, "klaudiush")

	settings := buildClaudeHookSettings("", false)
	if settings == "" {
		t.Fatal("expected Claude hook settings")
	}
	if !strings.Contains(settings, "klaudiush --hook-type PreToolUse") {
		t.Fatalf("settings missing klaudiush command: %s", settings)
	}

	var decoded struct {
		Hooks map[string][]claudeHookEntry `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &decoded); err != nil {
		t.Fatalf("settings is not JSON: %v\n%s", err, settings)
	}
	entries := decoded.Hooks["PreToolUse"]
	if len(entries) != 1 || len(entries[0].Hooks) != 1 {
		t.Fatalf("PreToolUse hooks = %+v, want one klaudiush hook", entries)
	}
	if got := entries[0].Hooks[0].Type; got != "command" {
		t.Errorf("hook type = %q, want command", got)
	}
}

func TestBuildClaudeHookSettings_KeepsApprovalHook(t *testing.T) {
	makeFakeHookBinary(t, "klaudiush")

	settings := buildClaudeHookSettings("127.0.0.1:12345", true)
	if !strings.Contains(settings, "klaudiush --hook-type PreToolUse") {
		t.Fatalf("settings missing klaudiush command: %s", settings)
	}
	if !strings.Contains(settings, "http://127.0.0.1:12345/hooks/pre-tool-use") {
		t.Fatalf("settings missing approval hook: %s", settings)
	}
}
