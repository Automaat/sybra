package workflow

import (
	"context"

	"github.com/Automaat/sybra/internal/project"
)

// These neutral collaborators preserve the historical partial-engine behavior
// for NewTestEngine without forcing production code to retain nil branches for
// dependencies that NewEngine guarantees.
type skippedBranchSyncer struct{}

func (skippedBranchSyncer) SyncTaskBranch(context.Context, string) (string, error) {
	return syncResultSkipped, nil
}

type emptyCheckConfigGetter struct{}

func (emptyCheckConfigGetter) CodegenCommands(context.Context, string) []string { return nil }
func (emptyCheckConfigGetter) VerifyCommands(context.Context, string) []string  { return nil }
func (emptyCheckConfigGetter) SetupCommands(context.Context, string) []string   { return nil }
func (emptyCheckConfigGetter) FocusedChecks(context.Context, string) []project.FocusedCheck {
	return nil
}

type emptyManualTestConfigGetter struct{}

func (emptyManualTestConfigGetter) ManualTestConfig(string) ManualTestInfo {
	return ManualTestInfo{}
}

type unlimitedCostBudgetChecker struct{}

func (unlimitedCostBudgetChecker) CheckTaskCostBudget(string) error { return nil }
