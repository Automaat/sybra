package agent

import (
	"slices"
	"testing"
)

func TestProcessOwnerFromEnvAssignments(t *testing.T) {
	t.Parallel()

	got := processOwnerFromEnvAssignments([]string{
		"PATH=/bin",
		processOwnerFlagEnv + "=" + processOwnerFlagTrue,
		processAgentIDEnv + "=agent-1",
		processTaskIDEnv + "=task-1",
		processAgentModeEnv + "=headless",
		"NOISE",
	})
	want := processOwner{AgentID: "agent-1", TaskID: "task-1", Mode: "headless"}
	if got != want {
		t.Fatalf("owner = %+v, want %+v", got, want)
	}

	for name, assignments := range map[string][]string{
		"missing_marker": {processAgentIDEnv + "=agent-1", processAgentModeEnv + "=headless"},
		"bad_marker":     {processOwnerFlagEnv + "=0", processAgentIDEnv + "=agent-1", processAgentModeEnv + "=headless"},
		"missing_agent":  {processOwnerFlagEnv + "=" + processOwnerFlagTrue, processAgentModeEnv + "=headless"},
		"missing_mode":   {processOwnerFlagEnv + "=" + processOwnerFlagTrue, processAgentIDEnv + "=agent-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := processOwnerFromEnvAssignments(assignments); got != (processOwner{}) {
				t.Fatalf("owner = %+v, want empty", got)
			}
		})
	}
}

func TestProcessOwnerFromAnyEnv_FallsBackToMCPOwner(t *testing.T) {
	t.Parallel()

	got := processOwnerFromAnyEnv([]string{
		mcpOwnerFlagEnv + "=" + mcpOwnerFlagTrue,
		mcpAgentIDEnv + "=agent-1",
		mcpTaskIDEnv + "=task-1",
		mcpAgentModeEnv + "=headless",
	})
	want := processOwner{AgentID: "agent-1", TaskID: "task-1", Mode: "headless"}
	if got != want {
		t.Fatalf("owner = %+v, want %+v", got, want)
	}
}

func TestInjectProcessOwnerEnv(t *testing.T) {
	t.Parallel()

	cfg := injectProcessOwnerEnv(RunConfig{
		ExtraEnv: []string{
			processAgentIDEnv + "=stale-agent",
			"PATH=/bin",
			processTaskIDEnv + "=stale-task",
		},
	}, processOwner{AgentID: "agent-1", TaskID: "task-1", Mode: "headless"})

	for _, want := range []string{
		processOwnerFlagEnv + "=" + processOwnerFlagTrue,
		processAgentIDEnv + "=agent-1",
		processTaskIDEnv + "=task-1",
		processAgentModeEnv + "=headless",
	} {
		if !slices.Contains(cfg.ExtraEnv, want) {
			t.Fatalf("ExtraEnv = %v, missing %q", cfg.ExtraEnv, want)
		}
	}
	for _, bad := range []string{
		processAgentIDEnv + "=stale-agent",
		processTaskIDEnv + "=stale-task",
	} {
		if slices.Contains(cfg.ExtraEnv, bad) {
			t.Fatalf("ExtraEnv = %v, stale entry %q survived", cfg.ExtraEnv, bad)
		}
	}
}

func TestNormalizeObservedProcessPath(t *testing.T) {
	t.Parallel()

	if got := normalizeObservedProcessPath("/tmp/task-1 (deleted)"); got != "/tmp/task-1" {
		t.Fatalf("normalizeObservedProcessPath(deleted) = %q", got)
	}
	if got := normalizeObservedProcessPath(" /tmp/task-1 "); got != "/tmp/task-1" {
		t.Fatalf("normalizeObservedProcessPath(trim) = %q", got)
	}
}
