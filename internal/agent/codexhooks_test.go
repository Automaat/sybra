package agent

import (
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
	dir := t.TempDir()
	fake := filepath.Join(dir, "sybra-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake sybra-cli: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
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

func TestBuildCodexHookArgs_NoOutOfScopeEvents(t *testing.T) {
	makeFakeSybraCLI(t)

	args := buildCodexHookArgs("task-abc123")
	for _, arg := range args {
		if strings.Contains(arg, "PreToolUse") || strings.Contains(arg, "PostToolUse") {
			t.Errorf("hook args contain out-of-scope event; arg=%q", arg)
		}
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

func TestBuildCodexHookArgs_MissingBinaryReturnsNil(t *testing.T) {
	// Point PATH to an empty dir with no sybra-cli.
	t.Setenv("PATH", t.TempDir())

	args := buildCodexHookArgs("task-abc123")
	if len(args) != 0 {
		t.Errorf("expected nil when sybra-cli missing; got %v", args)
	}
}

func TestBuildCodexHookArgs_CommandContainsTaskID(t *testing.T) {
	makeFakeSybraCLI(t)

	taskID := "task-xyz789"
	args := buildCodexHookArgs(taskID)

	// Each hook override value must embed the task ID.
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) {
			if !strings.Contains(args[i+1], taskID) {
				t.Errorf("-c value at index %d does not contain taskID %q; value=%q", i+1, taskID, args[i+1])
			}
		}
	}
}
