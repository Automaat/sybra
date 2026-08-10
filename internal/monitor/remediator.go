package monitor

import (
	"context"
	"fmt"
	"slices"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
)

// taskAPI is the slice of task.Manager the remediator + service needs. Keeps
// fakes in tests one-line shims and avoids accidental coupling.
type taskAPI interface {
	List() ([]task.Task, error)
	Get(id string) (task.Task, error)
	UpdateBy(id, actor string, u task.Update) (task.Task, error)
	ApplyStatusEffect(id string, eff task.StatusEffect) (task.Task, error)
	UpdateRunBy(taskID, actor, agentID string, patch task.RunPatch) error
}

// remediator runs the in-process actions for anomalies that don't need LLM
// judgment. It is split out from service.go so the service stays focused on
// orchestration.
type remediator struct {
	tasks            taskAPI
	recoverLostAgent func(context.Context, string)
	fetchPRState     func(repo string, number int) (github.PRState, error)
	landClosedPR     func(context.Context, string, int, string) error
}

func newRemediator(
	t taskAPI,
	recoverLostAgent func(context.Context, string),
	fetchPRState func(repo string, number int) (github.PRState, error),
	landClosedPR func(context.Context, string, int, string) error,
) *remediator {
	if fetchPRState == nil {
		fetchPRState = github.FetchPRState
	}
	return &remediator{
		tasks:            t,
		recoverLostAgent: recoverLostAgent,
		fetchPRState:     fetchPRState,
		landClosedPR:     landClosedPR,
	}
}

// Apply executes the action for one anomaly. Returns a label suitable for the
// Report.Remediated slice on success, or an error on failure. The caller logs
// errors but does not abort the cycle on them.
func (r *remediator) Apply(ctx context.Context, a Anomaly) (string, error) {
	switch a.Kind {
	case KindLostAgent:
		return r.resetLostAgent(ctx, a)
	case KindUntriaged:
		return r.tagUntriaged(a)
	case KindStuckHumanBlocked:
		if isHumanRequiredStuck(a) {
			return r.remediateHumanRequiredStuck(ctx, a)
		}
		return "", fmt.Errorf("remediator: stuck_human_blocked remediation requires human-required status")
	default:
		return "", fmt.Errorf("remediator: kind %q has no in-process action", a.Kind)
	}
}

func (r *remediator) resetLostAgent(ctx context.Context, a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("lost_agent without task id")
	}
	// Do not mark the run stopped here. The durable attempt controller must
	// first re-observe the process/session and preserve workspace state; only
	// its reconciled terminal path may release ownership or allow replacement.
	upd := task.Update{
		StatusReason: task.Ptr("monitor: agent lost; recovery will resume"),
	}
	if _, err := r.tasks.UpdateBy(a.TaskID, "monitor.remediator.reset_lost_agent", upd); err != nil {
		return "", fmt.Errorf("mark lost_agent task %s for recovery: %w", a.TaskID, err)
	}
	if r.recoverLostAgent != nil {
		r.recoverLostAgent(ctx, a.TaskID)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

func (r *remediator) tagUntriaged(a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("untriaged without task id")
	}
	upd := task.Update{
		StatusReason: task.Ptr("monitor: awaiting triage"),
	}
	if _, err := r.tasks.UpdateBy(a.TaskID, "monitor.remediator.tag_untriaged", upd); err != nil {
		return "", fmt.Errorf("tag untriaged task %s: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// remediateHumanRequiredStuck refreshes the status reason on a task that is
// already human-required and has exceeded its dwell budget. Updating the task
// file stamps a new UpdatedAt, resetting the dwell timer and suppressing
// repeated meta-task creation for the same block.
//
// When the anomaly's stall correlates with a known, already-tracked
// lost_agent investigation (detectStuckHumanBlocked's
// known_lost_agent_investigation evidence), this instead auto-retries the
// task once — see retryKnownLostAgentStuck.
func (r *remediator) remediateHumanRequiredStuck(ctx context.Context, a Anomaly) (string, error) {
	if a.TaskID == "" {
		return "", fmt.Errorf("stuck_human_blocked without task id")
	}
	t, err := r.tasks.Get(a.TaskID)
	if err != nil {
		return "", fmt.Errorf("inspect stuck_human_blocked task %s: %w", a.TaskID, err)
	}
	if merged, err := r.landMergedPR(ctx, t); err != nil {
		return "", err
	} else if merged {
		return string(a.Kind) + ":merged:" + a.TaskID, nil
	}
	if known, _ := a.Evidence["known_lost_agent_investigation"].(bool); known {
		// Umbrella trackers run no agent of their own — their in-progress is a
		// rollup of their children (app_umbrella_gate.go), not a dispatchable
		// unit of work. Flipping one straight to in-progress here bypasses the
		// only umbrella-guarded dispatch choke point (agentorch.startAgent) and
		// re-triggers the exact bug #2610 fixed. Mirrors detectLostAgents.
		if humanReviewVerdict(a) == "human" || t.Blocker.Kind == blocker.KindTamperDetected || t.TaskType == task.TaskTypeUmbrella {
			return r.refreshHumanRequiredStuck(a)
		}
		return r.retryKnownLostAgentStuck(a, t)
	}
	return r.refreshHumanRequiredStuck(a)
}

func (r *remediator) landMergedPR(ctx context.Context, t task.Task) (bool, error) {
	if t.ProjectID == "" || t.PRNumber == 0 {
		return false, nil
	}
	state, err := r.fetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		return false, err
	}
	if state.State != "MERGED" {
		return false, nil
	}
	if r.landClosedPR == nil {
		return false, nil
	}
	if err := r.landClosedPR(ctx, t.ID, t.PRNumber, state.State); err != nil {
		return false, fmt.Errorf("land merged PR task %s: %w", t.ID, err)
	}
	return true, nil
}

func (r *remediator) refreshHumanRequiredStuck(a Anomaly) (string, error) {
	// Empty update: preserve existing StatusReason; Marshal stamps new UpdatedAt.
	upd := task.Update{}
	if _, err := r.tasks.UpdateBy(a.TaskID, "monitor.remediator.refresh_human_required_stuck", upd); err != nil {
		return "", fmt.Errorf("tag human-required task %s stalled: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":" + a.TaskID, nil
}

// retryKnownLostAgentStuck moves a task out of human-required back to
// in-progress when its stall already has an open, monitor-filed lost_agent
// investigation tracking the root cause — no point leaving it parked for a
// human when recovery can retry it and the finding is already recorded
// elsewhere. Stamps monitorAutoRetriedTag so a second stall on the same task
// (the auto-retry did not help) falls back to the normal human-review path
// instead of bouncing between in-progress and human-required forever.
func (r *remediator) retryKnownLostAgentStuck(a Anomaly, t task.Task) (string, error) {
	tags := append(slices.Clone(t.Tags), monitorAutoRetriedTag)
	if _, err := r.tasks.ApplyStatusEffect(a.TaskID, task.StatusEffect{
		Source:         "monitor.stuck-human-blocked.retry-known-lost-agent",
		ToStatus:       task.StatusInProgress,
		ExpectedStatus: t.Status,
		Extra: task.Update{
			StatusReason: task.Ptr("monitor: stall matches an already-tracked lost_agent investigation; auto-retrying"),
			Tags:         &tags,
		},
	}); err != nil {
		return "", fmt.Errorf("retry known lost_agent stuck task %s: %w", a.TaskID, err)
	}
	return string(a.Kind) + ":retry:" + a.TaskID, nil
}

func humanReviewVerdict(a Anomaly) string {
	verdict, _ := a.Evidence["human_review_verdict"].(string)
	return verdict
}

// isHumanRequiredStuck reports whether a is a stuck_human_blocked anomaly for
// a task already in human-required status. These are remediated in-process
// (status reason refreshed to reset the dwell timer) and must not reach the
// issue sink, which would create a redundant meta-task.
func isHumanRequiredStuck(a Anomaly) bool {
	if a.Kind != KindStuckHumanBlocked {
		return false
	}
	status, _ := a.Evidence["status"].(string)
	return status == string(task.StatusHumanRequired)
}
