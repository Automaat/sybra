package workflow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// execClearPlanArtifacts wipes the previous planning cycle's outputs before the
// next one runs: the task sidecars named in ClearSidecars, and the files in the
// agent's worktree matching ClearWorktreeGlob.
//
// Both halves are load-bearing. A replan reuses the same worktree, and the
// planning files are git-excluded rather than deleted, so clearing only the
// sidecar leaves the file for the next import to read straight back; unlinking
// only the file leaves the sidecar holding the old value, since a missing file
// makes importOneSidecar return without writing. Either way the next cycle
// serves the previous cycle's content, and the require_sidecar guards pass on
// it — they assert non-empty, and stale content is non-empty (#2191).
//
// Fails closed. Replanning onto a half-cleared cycle is how a human approves a
// plan against another plan's brief, or how a stale "no open decisions"
// auto-approves a cycle the planner never spoke for — so anything this step
// cannot clear escalates to a human rather than advancing.
func (e *Engine) execClearPlanArtifacts(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	kinds := step.Config.ClearSidecars
	globs := step.Config.ClearWorktreeGlobs
	if len(kinds) == 0 && len(globs) == 0 {
		return StepOutput{}, fmt.Errorf("clear_plan_artifacts: nothing configured to clear")
	}

	var failed []string
	for _, kind := range kinds {
		// Empty content is the delete (task.PlanningSidecarStore.Write).
		if err := e.tasks.WriteSidecar(taskID, kind, ""); err != nil {
			e.logger.Error("workflow.clear-plan-artifacts.sidecar", "task_id", taskID, "step", step.ID, "kind", kind, "err", err)
			failed = append(failed, kind+" sidecar")
		}
	}

	removed := 0
	for _, glob := range globs {
		n, err := e.clearWorktreeGlob(taskID, step, t, strings.TrimSpace(glob))
		removed += n
		if err != nil {
			failed = append(failed, err.Error())
		}
	}

	if len(failed) > 0 {
		reason := "replan blocked: could not clear " + strings.Join(failed, ", ") +
			" — the next cycle would re-import the previous plan's content"
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.clear-plan-artifacts.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.clear-plan-artifacts.blocked", "task_id", taskID, "step", step.ID, "failed", strings.Join(failed, ", "))
		// Escalate exactly as require_sidecar does: flip the status and let the
		// step's human-required edge halt the workflow. errStepParked would be
		// wrong — its contract is that the step already persisted its own
		// ExecWaiting, and returning it without having done so strands the
		// execution at a step nothing resumes, wedging the very task this
		// escalation exists to protect.
		return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
	}

	e.logger.Info("workflow.clear-plan-artifacts", "task_id", taskID, "step", step.ID,
		"sidecars", len(kinds), "files_removed", removed)
	return StepOutput{Status: "completed", Output: fmt.Sprintf("cleared %d sidecars, %d files", len(kinds), removed)}, nil
}

// clearWorktreeGlob unlinks the agent-written planning files.
//
// A worktree dir the engine cannot resolve is reported rather than skipped: the
// files may well still be there, and this step's entire job is knowing that
// they are gone.
func (e *Engine) clearWorktreeGlob(taskID string, step *Step, t TaskInfo, glob string) (int, error) {
	if glob == "" {
		return 0, nil
	}
	dir := ""
	if t.Workflow != nil {
		dir = strings.TrimSpace(t.Workflow.Variables[WorkflowVarDir])
	}
	if dir == "" {
		return 0, fmt.Errorf("worktree files matching %s (worktree dir unknown)", glob)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		// Only a genuinely absent worktree is safe to pass: it holds no stale
		// files to serve. Any other stat error (EACCES on a parent traverse,
		// ESTALE, EIO) means the files may be sitting right there — reading
		// that as "gone" reports success over a worktree still holding cycle
		// 1's decisions, which walks straight back into #2191's fail-open.
		if !errors.Is(statErr, fs.ErrNotExist) {
			return 0, fmt.Errorf("worktree files matching %s (stat %s: %w)", glob, dir, statErr)
		}
		e.logger.Info("workflow.clear-plan-artifacts.no-worktree", "task_id", taskID, "step", step.ID, "dir", dir)
		return 0, nil
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, glob))
	if globErr != nil {
		return 0, fmt.Errorf("worktree files matching %s (%w)", glob, globErr)
	}
	removed := 0
	var stuck []string
	for _, m := range matches {
		if rmErr := os.Remove(m); rmErr != nil && !os.IsNotExist(rmErr) {
			e.logger.Error("workflow.clear-plan-artifacts.remove", "task_id", taskID, "path", m, "err", rmErr)
			stuck = append(stuck, filepath.Base(m))
			continue
		}
		removed++
	}
	if len(stuck) > 0 {
		return removed, fmt.Errorf("worktree files %s", strings.Join(stuck, ", "))
	}
	return removed, nil
}
