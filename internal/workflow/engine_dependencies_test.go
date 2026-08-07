package workflow

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

func TestNewEngineRejectsIncompleteDependencies(t *testing.T) {
	t.Parallel()

	engine, err := NewEngine(nil, nil, nil, nil, Dependencies{})
	if engine != nil {
		t.Fatal("NewEngine returned an engine for incomplete dependencies")
	}
	if err == nil {
		t.Fatal("NewEngine accepted incomplete dependencies")
	}
	for _, want := range []string{
		"Store", "Tasks", "Agents", "Logger", "PR.Linker",
		"Execution.Worktrees", "Execution.Classifier", "Execution.AttemptWorktrees",
		"Execution.Verification", "Execution.VerificationCommands",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name missing %s", err, want)
		}
	}
}

func TestNewEngineAcceptsCompleteDependencies(t *testing.T) {
	t.Parallel()

	engine, err := NewEngine(
		newTestStore(t),
		newMemTasks(),
		newMockAgents(),
		discardLogger(),
		completeDependencies(),
	)
	if err != nil {
		t.Fatalf("NewEngine(complete) = %v", err)
	}
	if engine == nil {
		t.Fatal("NewEngine(complete) returned nil")
	}
}

func TestNewEngineRejectsTypedNilDependency(t *testing.T) {
	t.Parallel()

	deps := completeDependencies()
	var classifier *stubExecutionSurface
	deps.Execution.Classifier = classifier

	engine, err := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger(), deps)
	if engine != nil || err == nil {
		t.Fatalf("NewEngine(typed nil classifier) = (%v, %v), want (nil, error)", engine, err)
	}
	if !strings.Contains(err.Error(), "Execution.Classifier") {
		t.Fatalf("error %q does not name typed-nil classifier", err)
	}
}

func TestNewTestEngineAllowsFocusedCollaborators(t *testing.T) {
	t.Parallel()

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	worktrees := stubExecutionSurface{}
	engine.SetWorktreeGetter(worktrees)
	if engine.execution.Worktrees != worktrees {
		t.Fatal("focused test collaborator was not installed")
	}
}

type stubExecutionSurface struct{}

func (stubExecutionSurface) GetWorktreePath(string) (string, bool) { return "", false }
func (stubExecutionSurface) AppendReimplementNote(context.Context, string, string, string, string) error {
	return nil
}
func (stubExecutionSurface) SyncTaskBranch(context.Context, string) (string, error) {
	return "noop", nil
}
func (stubExecutionSurface) CodegenCommands(context.Context, string) []string { return nil }
func (stubExecutionSurface) VerifyCommands(context.Context, string) []string  { return nil }
func (stubExecutionSurface) SetupCommands(context.Context, string) []string   { return nil }
func (stubExecutionSurface) FocusedChecks(context.Context, string) []project.FocusedCheck {
	return nil
}
func (stubExecutionSurface) ManualTestConfig(string) ManualTestInfo     { return ManualTestInfo{} }
func (stubExecutionSurface) ClassifyTask(context.Context, string) error { return nil }
func (stubExecutionSurface) CheckTaskCostBudget(string) error           { return nil }
func (stubExecutionSurface) PrepareAttempt(string, string) (dir, branch string, err error) {
	return "", "", nil
}
func (stubExecutionSurface) PromoteAttempt(string, string, string) (string, error) {
	return "", nil
}
func (stubExecutionSurface) CleanupAttempts(string, []string) {}
func (stubExecutionSurface) PrepareVerification(context.Context, string, string, string) (VerificationWorkspace, error) {
	return VerificationWorkspace{}, nil
}
func (stubExecutionSurface) FinalizeVerification(context.Context, VerificationWorkspace, []string, string) error {
	return nil
}
func (stubExecutionSurface) ValidateVerification(context.Context, VerificationWorkspace) error {
	return nil
}
func (stubExecutionSurface) ReleaseVerification(VerificationWorkspace) {}
func (stubExecutionSurface) RunVerificationCommand(context.Context, string, string, string, []string, io.Writer) error {
	return nil
}

func completeDependencies() Dependencies {
	execution := stubExecutionSurface{}
	return Dependencies{
		PR: completePRSurface(),
		Execution: ExecutionSurface{
			Worktrees:            execution,
			SidecarDir:           func(string) (string, error) { return "", nil },
			AttemptNotes:         execution,
			BranchSyncer:         execution,
			Checks:               execution,
			ManualTests:          execution,
			Classifier:           execution,
			CostBudget:           execution,
			AttemptWorktrees:     execution,
			Verification:         execution,
			VerificationCommands: execution,
		},
	}
}
