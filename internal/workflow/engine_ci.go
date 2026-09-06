package workflow

import (
	"context"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
)

type ciConfigGetter interface {
	CIConfig(context.Context, string) (*project.CIConfig, error)
}

type prReadyMarker interface {
	MarkReady(context.Context, string, int) error
}

func (e *Engine) ciPolicy(taskID string) (*project.CIConfig, error) {
	getter, ok := e.execution.Checks.(ciConfigGetter)
	if !ok {
		return &project.CIConfig{}, nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	return getter.CIConfig(ctx, taskID)
}

func (e *Engine) ciEnabled(taskID string) bool {
	cfg, err := e.ciPolicy(taskID)
	// Unknown policy must never loosen verification. Mutation steps read the
	// error-bearing snapshot themselves and stop before external effects.
	return err != nil || (cfg != nil && cfg.Enabled)
}

// Start CI while independent review and testing proceed locally. Deliberately
// do NOT link the draft to task.PRNumber: that field hands ownership to the PR
// monitor and skips the pre-PR review path. Branch identity makes creation
// idempotent across restarts; the ordinary PR-tail adopts it after local gates.
func (e *Engine) execStartCI(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	cfg, err := e.ciPolicy(taskID)
	if err != nil {
		return StepOutput{}, err
	}
	if cfg == nil || !cfg.Enabled || t.PRNumber != 0 {
		return stepDone(step, "skipped: early CI not required")
	}
	if len(github.NormalizeRequiredChecks(cfg.RequiredChecks)) == 0 {
		return e.humanRequiredPR(taskID, step, "checks.ci requires an explicit non-empty required_checks list")
	}
	if out, err := e.execPushBranch(taskID, step, wfExec, t); err != nil || out.Status != "completed" {
		return out, err
	}
	// Push can park the task while still returning a completed mechanical
	// step. Re-read status before creating any external artifact.
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		return StepOutput{}, err
	}
	if fresh.Status != t.Status {
		return stepDone(step, "early CI deferred after push")
	}
	return e.createPR(taskID, step, wfExec, fresh, true)
}

// publishCIDraft runs only at the ordinary PR tail, after review/testing and
// require_evidence. CI may still be pending: the existing PR monitor owns the
// asynchronous wait/fix/merge loop, with strict required-check validation.
func (e *Engine) publishCIDraft(taskID string, t TaskInfo, number int) error {
	if !e.ciEnabled(taskID) || t.ProjectType != "pet" {
		return nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	meta, err := e.pr.MetaFetcher.FetchPRMeta(ctx, t.ProjectID, number)
	if err != nil {
		return err
	}
	// Generic push_branch is also used by pr-fix *before* its deterministic
	// verification tail. Only the initial draft-publication boundary consumes
	// local review/testing evidence; ordinary fix pushes remain in that tail.
	if !meta.IsDraft {
		return nil
	}
	if head := e.currentHeadSHA(taskID); head == "" || meta.HeadSHA != head {
		return &ClientError{status: 409, msg: "CI draft head differs from locally verified revision"}
	}
	fresh, err := e.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	out, err := e.execRequireEvidence(taskID, &Step{ID: "require_evidence", Type: StepRequireEvidence}, fresh)
	if err != nil {
		return err
	}
	if out.Output != "complete" {
		return &ClientError{status: 409, msg: "CI draft publication requires current completion evidence"}
	}
	marker, ok := e.pr.Creator.(prReadyMarker)
	if !ok {
		return &ClientError{status: 503, msg: "CI draft publication is unavailable"}
	}
	return marker.MarkReady(ctx, t.ProjectID, number)
}
