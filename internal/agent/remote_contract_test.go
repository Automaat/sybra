package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/providerid"
)

func TestRemoteRunSpecRoundTripPreservesExecutionIntent(t *testing.T) {
	deadline := time.Date(2026, 8, 12, 20, 0, 0, 0, time.UTC)
	start := ExecutionStart{
		Spec: ExecutionSpec{
			ID: "agent-123", TaskID: "task-123", Mode: "headless", Provider: providerid.Codex,
			Model: "gpt-5.6-codex", ReasoningEffort: "high",
		},
		Config: RunConfig{
			TaskID: "task-123", Role: RoleImplementation, Mode: "headless",
			Prompt: "Implement the accepted plan", AllowedTools: []string{"Bash", "Read"},
			Dir: "/leader/private/worktrees/task-123", SidecarDir: "/leader/private/tasks",
			IntentID: "dispatch:task-123:17", TaskGeneration: 17,
			RequirePermissions: true, HeadlessPermissionMode: "auto",
			Model: "gpt-5.6-codex", MaxTurns: 150, BashTimeoutMs: 120000,
			HeadlessSteerable: true, ForkSubagent: true, RetryWatchdog: 8,
			FallbackModel: "gpt-5.5", RequestedSkill: "ship-issue", SkillExecutionMode: "native",
			SeedWorkingMemory: true, ResumeSessionID: "sensitive-session",
			OutputSchema: `{"type":"object"}`,
		},
	}
	want, err := RemoteRunSpec(start, RemoteRunMetadata{
		BuildVersion: "1.2.3", RunID: "run-123", EffectID: "17:4:implement:0",
		WorkflowID: "ship", WorkflowGeneration: 6, WorkflowStepID: "implement",
		Deadline: deadline, WorkspaceBaseSHA: strings.Repeat("a", 40), WorkspaceBaseRef: "refs/heads/main",
		WorkspaceRoots: []executioncontract.LogicalRoot{
			executioncontract.RootWorktree, executioncontract.RootSidecar,
			executioncontract.RootArtifact, executioncontract.RootWorkingMemory,
		},
		Environment: []executioncontract.EnvironmentBinding{
			{Name: "FEATURE_MODE", Value: "remote"},
			{Name: "TASK_GRANT", SecretRef: &executioncontract.SecretRef{Name: "run/task-123/grant"}},
		},
		ExpectedOutputs: []executioncontract.ExpectedOutput{{
			Name: "git-diff", Kind: "git_bundle", Root: executioncontract.RootArtifact,
			Path: "changes/run.bundle", Required: true, Sensitivity: executioncontract.SensitivityInternal,
		}},
		Resources: executioncontract.ResourceLimits{CPUMillis: 2000, MemoryBytes: 4 << 30},
	})
	if err != nil {
		t.Fatalf("RemoteRunSpec: %v", err)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := executioncontract.DecodeRunSpec(encoded)
	if err != nil {
		t.Fatalf("DecodeRunSpec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got: %#v\nwant: %#v", got, want)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"/leader/private", "SidecarDir", "BeforeStart", "ExtraEnv"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("wire spec leaked %q: %s", forbidden, serialized)
		}
	}
	if got.Fence.TaskGeneration != 17 || got.Fence.WorkflowGeneration != 6 || got.Options.ResumeSessionID != "sensitive-session" ||
		got.Resources.MaxTurns != 150 || got.Resources.BashTimeoutMillis != 120000 {
		t.Fatalf("execution intent lost: %+v", got)
	}
}

func TestRemoteRunSpecRejectsProcessLocalInputs(t *testing.T) {
	validMetadata := RemoteRunMetadata{
		BuildVersion: "test", RunID: "run", EffectID: "effect", WorkflowID: "workflow", Deadline: time.Now().Add(time.Hour),
		WorkspaceBaseSHA: strings.Repeat("a", 40), WorkspaceBaseRef: "refs/heads/main",
		WorkspaceRoots: []executioncontract.LogicalRoot{executioncontract.RootWorktree},
	}
	base := ExecutionStart{
		Spec:   ExecutionSpec{ID: "agent", TaskID: "task", Provider: providerid.Claude, Model: "sonnet"},
		Config: RunConfig{TaskID: "task", Role: RoleImplementation, Prompt: "prompt", IntentID: "intent"},
	}
	withEnvironment := base
	withEnvironment.Config.ExtraEnv = []string{"TOKEN=secret"}
	if _, err := RemoteRunSpec(withEnvironment, validMetadata); err == nil {
		t.Fatal("raw environment accepted")
	}
	withCallback := base
	withCallback.Config.BeforeStart = func(string) error { return nil }
	if _, err := RemoteRunSpec(withCallback, validMetadata); err == nil {
		t.Fatal("process-local callback accepted")
	}
}
