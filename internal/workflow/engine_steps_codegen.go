package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Automaat/sybra/internal/project"
)

type codegenGateReport struct {
	Commands   []string `json:"commands"`
	Skipped    []string `json:"skipped,omitempty"`
	Committed  bool     `json:"committed"`
	OutputTail string   `json:"outputTail,omitempty"`
}

func (e *Engine) execCodegenGate(taskID string, step *Step) (StepOutput, error) {
	if e.checks == nil {
		return stepDone(step, "skipped: no check config getter")
	}

	timeout := e.verifyTimeout
	if timeout <= 0 {
		timeout = verifyChecksDefaultTimeout
	}
	cmds := e.checks.CodegenCommands(e.ctx, taskID)
	if len(cmds) == 0 {
		return stepDone(step, "skipped: no codegen commands configured")
	}
	if e.worktrees == nil {
		return stepDone(step, "skipped: no worktree getter configured")
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return stepDone(step, "skipped: no worktree for task")
	}

	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	maybeMiseTrust(ctx, wtPath)

	report := codegenGateReport{Commands: cmds}
	tail := &boundedTail{max: verifyChecksMaxOutput}
	for _, raw := range cmds {
		failedCmd, output, runErr := e.runVerifyCommands(ctx, taskID, wtPath, []string{raw})
		_, _ = io.WriteString(tail, output)

		if failedCmd != "" && verifyMissingToolchainRe.MatchString(output) {
			report.Skipped = append(report.Skipped, raw)
			e.logger.Warn("workflow.codegen-gate.skip-toolchain",
				"task_id", taskID, "cmd", trimDiffLine(raw))
			continue
		}

		if runErr != nil {
			if errors.Is(runErr, context.Canceled) && e.ctx.Err() != nil && !errors.Is(runErr, context.DeadlineExceeded) {
				e.logger.Warn("workflow.codegen-gate.canceled", "task_id", taskID, "err", runErr)
				return stepDone(step, "skipped: context canceled")
			}
			reason := fmt.Sprintf(
				"codegen gate exceeded the time budget (%s) before the worktree was clean — rerun the repo's format/codegen commands and commit the result",
				timeout)
			if !errors.Is(runErr, context.DeadlineExceeded) {
				reason = "codegen gate could not finish cleanly: " + trimDiffLine(runErr.Error())
			}
			return e.flagCodegenGate(taskID, step, reason, runErr.Error())
		}

		if failedCmd != "" {
			reason := "codegen gate failed while running " + trimDiffLine(failedCmd) +
				" — rerun the repo's format/codegen commands and commit the resulting changes"
			return e.flagCodegenGate(taskID, step, reason, failedCmd)
		}
	}

	committed, err := project.CheckpointCommit(ctx, wtPath, "chore(codegen): apply codegen and goimports")
	if err != nil {
		reason := "codegen gate could not checkpoint generated changes: " + trimDiffLine(err.Error())
		return e.flagCodegenGate(taskID, step, reason, err.Error())
	}

	report.Committed = committed
	report.OutputTail = tailString(tail.String(), verifyChecksOutputTail)
	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "codegen-gate.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.codegen-gate.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}

	if committed {
		e.logger.Info("workflow.codegen-gate.committed", "task_id", taskID, "commands", len(cmds))
		return stepDone(step, "committed")
	}
	e.logger.Info("workflow.codegen-gate.clean", "task_id", taskID, "commands", len(cmds))
	return stepDone(step, "clean")
}

func (e *Engine) flagCodegenGate(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("codegen-gate: set human-required: %w", statusErr)
	}
	e.logger.Warn("workflow.codegen-gate.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}
