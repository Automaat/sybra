package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/workflow/failureclassify"
)

type codegenGateReport struct {
	Commands   []string `json:"commands"`
	Skipped    []string `json:"skipped,omitempty"`
	Committed  bool     `json:"committed"`
	OutputTail string   `json:"outputTail,omitempty"`
}

func (e *Engine) execCodegenGate(taskID string, step *Step) (StepOutput, error) {
	timeout := resolveWorkflowCheckTimeout(e.verifyTimeout)
	cmds := e.execution.Checks.CodegenCommands(e.ctx, taskID)
	if len(cmds) == 0 {
		return stepDone(step, "skipped: no codegen commands configured")
	}
	if e.execution.Worktrees == nil {
		return stepDone(step, "skipped: no worktree getter configured")
	}
	wtPath, ok := e.execution.Worktrees.GetWorktreePath(taskID)
	if !ok {
		return stepDone(step, "skipped: no worktree for task")
	}
	if timeout != e.verifyTimeout && e.verifyTimeout > 0 {
		e.logger.Info("workflow.codegen-gate.timeout-scaled",
			"task_id", taskID, "base", e.verifyTimeout.String(), "effective", timeout.String())
	}
	if e.verifyTimeout <= 0 && timeout != verifyChecksDefaultTimeout {
		e.logger.Info("workflow.codegen-gate.timeout-scaled",
			"task_id", taskID, "base", verifyChecksDefaultTimeout.String(), "effective", timeout.String())
	}

	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	// Verification commands run with an ephemeral HOME. Trusting the mise
	// config in the operator's HOME does not carry into that sandbox, so a
	// valid `mise exec -- ...` codegen command is rejected as untrusted. Use
	// the same contained trust preparation as verify_checks.
	e.maybeMiseTrustContained(ctx, taskID, wtPath, wtPath)

	report := codegenGateReport{Commands: cmds}
	tail := &boundedTail{max: verifyChecksMaxOutput}
	for _, raw := range cmds {
		failedCmd, output, runErr := e.runVerifyCommands(ctx, taskID, wtPath, []string{raw})
		_, _ = io.WriteString(tail, output)

		if failedCmd != "" && failureclassify.IsMissingToolchain(output) {
			reason := "codegen gate could not run " + trimDiffLine(failedCmd) +
				" because the configured toolchain is missing from PATH — rerun setup or install the missing tool, then retry"
			return e.flagCodegenGate(taskID, step, reason, tailString(output, 400))
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
			return e.flagCodegenGate(taskID, step, reason, tailString(output, 400))
		}
	}

	committed, err := project.CheckpointCommit(ctx, wtPath, "chore(codegen): apply generated changes")
	if err != nil {
		reason := "codegen gate could not checkpoint generated changes: " + trimDiffLine(err.Error())
		return e.flagCodegenGate(taskID, step, reason, err.Error())
	}

	report.Committed = committed
	report.OutputTail = tail.String()
	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "codegen-gate.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.codegen-gate.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}

	e.recordEvidence(taskID, step.ID, evidenceCriterionCodegenGate, evidence.ProofDeterministicCheck,
		0, strings.Join(cmds, " && "), report.OutputTail)
	if committed {
		e.logger.Info("workflow.codegen-gate.committed", "task_id", taskID, "commands", len(cmds))
		return stepDone(step, "committed")
	}
	e.logger.Info("workflow.codegen-gate.clean", "task_id", taskID, "commands", len(cmds))
	return stepDone(step, "clean")
}

func (e *Engine) flagCodegenGate(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("codegen-gate: set human-required: %w", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionCodegenGate, evidence.ProofDeterministicCheck, 1, "", detail)
	e.logger.Warn("workflow.codegen-gate.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}
