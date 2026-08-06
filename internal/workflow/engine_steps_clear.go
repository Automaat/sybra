package workflow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/taskstatus"
)

// execClearPlanArtifacts wipes the previous planning cycle's outputs before the
// next one runs: the task sidecars named in ClearSidecars, and the files in the
// agent's worktree matching ClearWorktreeGlobs.
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
		e.logger.Warn("workflow.clear-plan-artifacts.blocked", "task_id", taskID, "step", step.ID, "failed", strings.Join(failed, ", "))
		if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
			e.logger.Error("workflow.clear-plan-artifacts.status", "task_id", taskID, "err", statusErr)
			// The status flip is what halts the workflow at the human-required
			// edge; if it fails, that edge never fires and the unconditional
			// goto:plan would carry on over the very half-cleared cycle this
			// step exists to catch. Returning a hard error here is what stops
			// AdvanceStep instead — see engine_advance.go's execSyncStep path.
			return StepOutput{}, fmt.Errorf("clear_plan_artifacts: %s, and could not set human-required: %w", reason, statusErr)
		}
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
		// A blank entry in clear_worktree_globs is a config mistake, not a
		// no-op: silently succeeding here would let the "nothing configured
		// to clear" guard above pass on a list like ["  "] while clearing
		// nothing, exactly the half-cleared state this step exists to catch.
		return 0, errors.New("worktree glob is empty")
	}
	// Scratch artifacts live in the sidecar dir once one is seeded — verifier
	// roles cannot write the worktree (#2791) — so clear them where they
	// actually are. Falling back to the worktree keeps pre-#2791 executions,
	// whose files really are in the tree, clearable on replan.
	dir := ""
	if t.Workflow != nil {
		dir = strings.TrimSpace(t.Workflow.Variables[WorkflowVarSidecarDir])
		if dir == "" {
			dir = strings.TrimSpace(t.Workflow.Variables[WorkflowVarDir])
		}
	}
	if dir == "" {
		return 0, fmt.Errorf("scratch files matching %s (no sidecar or worktree dir known)", glob)
	}
	// List the dir rather than stat it. Only a genuinely absent worktree is safe
	// to pass, since it holds no stale files to serve; every other error has to
	// escalate. filepath.Glob reports a bad *pattern* but never a directory it
	// could not read, so an unreadable worktree yields zero matches and no
	// error, and this step would report "cleared" over cycle 1's decisions file
	// sitting right there. A stat cannot see that, and also passes for a
	// non-directory — the same fail-open through a different hole.
	if _, readErr := os.ReadDir(dir); readErr != nil {
		if !errors.Is(readErr, fs.ErrNotExist) {
			return 0, fmt.Errorf("worktree files matching %s (read %s: %w)", glob, dir, readErr)
		}
		e.logger.Info("workflow.clear-plan-artifacts.no-worktree", "task_id", taskID, "step", step.ID, "dir", dir)
		return 0, nil
	}
	// An absolute or traversing pattern makes filepath.Join escape the worktree
	// and unlink whatever it matches: '/etc/*' or '../*' would turn a cleanup
	// step into a delete primitive pointed anywhere this process can reach.
	// Builtins are trusted today; nothing should sit one edit away from that.
	if filepath.IsAbs(glob) || strings.Contains(glob, "..") || strings.ContainsAny(glob, `/\`) {
		return 0, fmt.Errorf("worktree glob %q must be a bare filename pattern", glob)
	}
	matches, globErr := filepath.Glob(filepath.Join(dir, glob))
	if globErr != nil {
		return 0, fmt.Errorf("worktree files matching %s (%w)", glob, globErr)
	}
	removed := 0
	var stuck []string
	for _, m := range matches {
		// Belt and braces for a symlinked or otherwise surprising match: only
		// unlink what genuinely sits directly in this worktree.
		if filepath.Dir(m) != filepath.Clean(dir) {
			return removed, fmt.Errorf("worktree glob %q matched %s outside %s", glob, m, dir)
		}
		rmErr := os.Remove(m)
		switch {
		case rmErr == nil:
			removed++
		case os.IsNotExist(rmErr):
			// Gone between Glob and Remove: nothing this call unlinked, so it
			// must not inflate the count callers log and report as cleared.
		default:
			e.logger.Error("workflow.clear-plan-artifacts.remove", "task_id", taskID, "path", m, "err", rmErr)
			stuck = append(stuck, filepath.Base(m))
		}
	}
	if len(stuck) > 0 {
		return removed, fmt.Errorf("worktree files %s", strings.Join(stuck, ", "))
	}
	return removed, nil
}
