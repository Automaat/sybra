package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// foreignBranchRefRe matches Sybra branch refs of the form
// `sybra/<slug>-<8hex>`. The trailing 8-hex group is the task ID; if it
// differs from the current task's ID, the plan is referencing another
// task's branch — likely cross-task LLM contamination during plan
// synthesis (see fa6919fc / a9375bad incident).
var foreignBranchRefRe = regexp.MustCompile(`sybra/[a-z0-9][a-z0-9-]*-([0-9a-f]{8})\b`)

// foreignWorktreePathRe matches Sybra worktree paths of the form
// `worktrees/<slug>-<8hex>`. Same contamination signal as branch refs.
var foreignWorktreePathRe = regexp.MustCompile(`worktrees/[a-z0-9][a-z0-9-]*-([0-9a-f]{8})\b`)

// execValidatePlan rejects synthesized plans that reference branches or
// worktree paths belonging to OTHER tasks. The fa6919fc → a9375bad
// incident showed that two parallel plan agents against the same project
// can confabulate a sibling task's identity into the synthesized plan,
// causing the implementation agent to commit to the wrong branch. Flips
// the task to human-required so the failure is visible instead of
// silently advancing into implementation on a contaminated plan.
//
// Designed to run AFTER require_plan so the plan field is guaranteed
// non-empty when this step fires. A trimmed-empty plan is treated as
// "nothing to validate" and the step passes — require_plan upstream
// already flipped the task in that case.
func (e *Engine) execValidatePlan(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	plan := t.Plan
	if strings.TrimSpace(plan) == "" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "plan empty (require_plan upstream should have flagged)"}, nil
	}

	foreign := collectForeignTaskIDs(plan, taskID)
	if len(foreign) == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "plan refs OK"}, nil
	}

	reason := fmt.Sprintf(
		"plan references foreign task ID(s) %s — likely cross-task contamination during plan synthesis. Re-dispatch the task or edit the plan manually before approving.",
		strings.Join(foreign, ", "),
	)
	if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
		e.logger.Error("workflow.validate-plan.status", "task_id", taskID, "err", statusErr)
	}
	e.logger.Warn("workflow.validate-plan.foreign-refs",
		"task_id", taskID, "foreign", strings.Join(foreign, ","))
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

// collectForeignTaskIDs scans plan markdown for branch refs and worktree
// paths whose trailing 8-hex task ID differs from currentID. Returns a
// sorted, deduplicated slice. Restricted to the two structural patterns
// (branch refs, worktree paths) so bare 8-hex strings — git short SHAs,
// hex constants in code samples — never trip the validator.
func collectForeignTaskIDs(plan, currentID string) []string {
	seen := make(map[string]struct{})
	for _, m := range foreignBranchRefRe.FindAllStringSubmatch(plan, -1) {
		if id := m[1]; id != currentID {
			seen[id] = struct{}{}
		}
	}
	for _, m := range foreignWorktreePathRe.FindAllStringSubmatch(plan, -1) {
		if id := m[1]; id != currentID {
			seen[id] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
