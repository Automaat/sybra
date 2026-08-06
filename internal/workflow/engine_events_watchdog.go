package workflow

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/watchdogreason"
)

type watchdogRecoveryKind string

const (
	watchdogRecoveryHang           watchdogRecoveryKind = "hang"
	watchdogRecoveryRewardHacking  watchdogRecoveryKind = "reward_hacking"
	watchdogRecoveryRateLimit      watchdogRecoveryKind = "rate_limit"
	watchdogRecoveryStop           watchdogRecoveryKind = "stop"
	watchdogRecoveryWorktreeRepair watchdogRecoveryKind = "worktree_repair"
)

type watchdogRecoverySpec struct {
	preflight func(*Engine, *TaskInfo, *Step) bool
	policy    boundedRetryPolicy
}

type watchdogRecoverySpecBuilder func(*Engine, *TaskInfo, *Step) (watchdogRecoverySpec, bool)

var watchdogRecoverySpecBuilders = map[watchdogRecoveryKind]watchdogRecoverySpecBuilder{
	watchdogRecoveryHang: func(e *Engine, _ *TaskInfo, _ *Step) (watchdogRecoverySpec, bool) {
		return e.watchdogHangRecoverySpec(), true
	},
	watchdogRecoveryRewardHacking: func(e *Engine, t *TaskInfo, step *Step) (watchdogRecoverySpec, bool) {
		return e.watchdogRewardHackingRecoverySpec(t, step)
	},
	watchdogRecoveryRateLimit: func(e *Engine, _ *TaskInfo, _ *Step) (watchdogRecoverySpec, bool) {
		return e.watchdogRateLimitRecoverySpec(), true
	},
	watchdogRecoveryStop: func(e *Engine, _ *TaskInfo, _ *Step) (watchdogRecoverySpec, bool) {
		return e.watchdogStopRecoverySpec(), true
	},
	watchdogRecoveryWorktreeRepair: func(e *Engine, _ *TaskInfo, _ *Step) (watchdogRecoverySpec, bool) {
		return e.worktreeRepairRecoverySpec(), true
	},
}

func (e *Engine) handleWatchdogRetries(t *TaskInfo, step *Step) bool {
	if e.handleWatchdogHangRetry(t, step) {
		return true
	}
	return e.handleWatchdogRewardHackingRetry(t, step)
}

func (e *Engine) handleWatchdogHangRetry(t *TaskInfo, step *Step) bool {
	return e.handleWatchdogRecoveryRetry(watchdogRecoveryHang, t, step)
}

func (e *Engine) handleWatchdogHangReadyPR(t *TaskInfo, step *Step) bool {
	if e.prStates == nil || t == nil || t.Workflow == nil || step == nil {
		return false
	}
	if t.ProjectID == "" || t.PRNumber <= 0 {
		return false
	}
	if t.Workflow.WorkflowID != "simple-task-implement" || step.ID != "implement" {
		return false
	}
	state, err := e.prStates.FetchPRState(t.ProjectID, t.PRNumber)
	if err != nil {
		e.logger.Warn("workflow.watchdog-hang.ready-pr.fetch", "task_id", t.ID, "pr", t.PRNumber, "err", err)
		return false
	}
	if !state.ReadyToMerge() {
		return false
	}

	delete(t.Workflow.Variables, watchdogReaskNoteVar)
	now := time.Now().UTC()
	t.Workflow.State = ExecCompleted
	t.Workflow.CompletedAt = &now
	t.Workflow.CurrentStep = ""
	t.Workflow.SetVar("cancel_reason", "watchdog hang: implementation superseded by linked PR already open and green")
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.watchdog-hang.ready-pr.persist", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "err", err)
		return true
	}
	if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.InReview, ""); err != nil {
		e.logger.Error("workflow.watchdog-hang.ready-pr.status", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "err", err)
		return true
	}
	e.logger.Info("workflow.watchdog-hang.ready-pr", "task_id", t.ID, "step", step.ID, "pr", t.PRNumber, "ci_status", state.CIStatus())
	return true
}

func isWatchdogHangReason(reason string) bool {
	return watchdogreason.IsHang(reason)
}

func buildWatchdogReaskNote(attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous run on this step was TERMINATED because it produced no output for an extended period (watchdog hang) — attempt %d of %d. A hang almost always means a command blocked: the full test suite, a foreground server that never backgrounds, an interactive prompt, or a wedged build.\n\n", attempt, maxWatchdogHangRetries)
	b.WriteString("To make forward progress this time:\n")
	b.WriteString("- Do NOT run the whole suite (`mise run verify`, `go test ./...`, full `npm` builds). Sybra runs codegen and the verify suite deterministically AFTER you finish — build and test only the narrow packages you changed.\n")
	b.WriteString("- Never launch a foreground long-running or interactive process; background any server and bound every command.\n")
	b.WriteString("- Commit and push incrementally so partial progress survives a restart.\n")
	b.WriteString("- If you are genuinely blocked, STOP and mark the task human-required with the specific blocker instead of looping.")
	return b.String()
}

type watchdogRewardHackingRetryProfile struct {
	max             int
	note            func(attempt int) string
	exhaustedReason func(attempts int) string
}

func (e *Engine) handleWatchdogRewardHackingRetry(t *TaskInfo, step *Step) bool {
	return e.handleWatchdogRecoveryRetry(watchdogRecoveryRewardHacking, t, step)
}

func rewardHackingRetryProfile(t *TaskInfo, step *Step) (watchdogRewardHackingRetryProfile, bool) {
	if t == nil || t.Workflow == nil || step == nil || step.Type != StepRunAgent || !isWatchdogRewardHackingReason(t.StatusReason) {
		return watchdogRewardHackingRetryProfile{}, false
	}
	switch step.Config.Role {
	case "fix-review":
		return watchdogRewardHackingRetryProfile{
			max:  maxWatchdogRewardHackingRetries,
			note: buildRewardHackingFixReviewReaskNote,
			exhaustedReason: func(attempts int) string {
				return fmt.Sprintf("watchdog: reward-hacking retry budget exhausted after %d clean re-dispatch(es) — review finding still unaddressed", attempts)
			},
		}, true
	case "implementation":
		return watchdogRewardHackingRetryProfile{
			max: maxWatchdogStopRetries,
			note: func(attempt int) string {
				return buildRewardHackingImplementationReaskNote(attempt, maxWatchdogStopRetries)
			},
			exhaustedReason: func(attempts int) string {
				return fmt.Sprintf("watchdog: reward-hacking retry budget exhausted after %d clean re-dispatches", attempts)
			},
		}, true
	case "plan", "plan-critic":
		return watchdogRewardHackingRetryProfile{
			max: maxWatchdogRewardHackingRetries,
			note: func(attempt int) string {
				return buildRewardHackingPlanningReaskNote(step.Config.Role, attempt, maxWatchdogRewardHackingRetries)
			},
			exhaustedReason: func(attempts int) string {
				return fmt.Sprintf("watchdog: reward-hacking retry budget exhausted after %d clean re-dispatch(es) — planning stage kept looping without converging", attempts)
			},
		}, true
	default:
		return watchdogRewardHackingRetryProfile{}, false
	}
}

func isWatchdogRewardHackingReason(reason string) bool {
	return watchdogreason.IsRewardHackingRetry(reason)
}

func watchdogRewardHackingRetryKey(stepID string) string {
	return watchdogRewardHackingRetryVarPrefix + stepID
}

func buildRewardHackingFixReviewReaskNote(attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous run on this step was TERMINATED because the watchdog detected a reward-hacking pattern — repeating the same non-editing action (reading/navigating) instead of making progress — attempt %d of %d.\n\n", attempt, maxWatchdogRewardHackingRetries)
	b.WriteString("The previous attempt stalled reading unrelated files; the code review sidecar already names the fix location. Read it, then edit that exact file directly — do not re-read unrelated files or repeat prior investigation.\n\n")
	b.WriteString("If you are genuinely blocked on understanding the finding, STOP and mark the task human-required with the specific blocker instead of looping.")
	return b.String()
}

func buildRewardHackingImplementationReaskNote(attempt, maxRetries int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous implementation run was TERMINATED because the watchdog detected a reward-hacking pattern — repeating the same investigation or command instead of changing code or approach — attempt %d of %d.\n\n", attempt, maxRetries)
	b.WriteString("Continue from the current worktree state, not from scratch:\n")
	b.WriteString("- Read `NOTES.md`, `git status --short`, and the current diff before more repo-wide exploration.\n")
	b.WriteString("- Reuse the progress already on this branch; change the code or command that failed instead of re-reading unrelated files.\n")
	b.WriteString("- Keep checks focused to the files you touched; do not fall back to the full suite.\n")
	b.WriteString("- If you are genuinely blocked, STOP and mark the task human-required with the specific blocker instead of looping.\n")
	return b.String()
}

func buildRewardHackingPlanningReaskNote(role string, attempt, maxRetries int) string {
	var b strings.Builder
	// Prompt prose, not a status: "plan-critique" is not a member of the
	// vocabulary, so typing this would promise a validity it does not have.
	stage := "planning"
	if role == "plan-critic" {
		stage = "plan-critique"
	}
	fmt.Fprintf(&b, "⚠️ Your previous %s run was TERMINATED because the watchdog detected a reward-hacking pattern — repeating navigation or rereads without advancing the planning artifacts — attempt %d of %d.\n\n", stage, attempt, maxRetries)
	b.WriteString("Refine the existing planning artifacts instead of restarting broad exploration:\n")
	if role == "plan-critic" {
		b.WriteString("- Start from the current plan contract/plan/brief and produce grounded critique against those artifacts.\n")
		b.WriteString("- Do not spend another turn re-reading unrelated source files unless the existing contract points to a real gap.\n")
	} else {
		b.WriteString("- Reuse the current plan research/decisions/brief/contract where possible and tighten the weak spot that caused the loop.\n")
		b.WriteString("- Do not restart repository-wide discovery unless the existing artifacts are missing a concrete file, symbol, or verification step.\n")
	}
	b.WriteString("- If the planning artifact is truly unusable, STOP and mark the task human-required with the specific blocker instead of looping.\n")
	return b.String()
}

func buildWatchdogReaskNoteForStep(t TaskInfo, step *Step, attempt int) string {
	if !isTestRunnerWatchdogStep(step) {
		return buildWatchdogReaskNote(attempt)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous adversarial test run was TERMINATED for watchdog hang before it produced a verdict — attempt %d of %d.\n\n", attempt, maxWatchdogHangRetries)
	b.WriteString("Before any further repo reading, start the declared manual_test surface and prove it is live.\n")
	if t.ManualTest.Kind != "" {
		fmt.Fprintf(&b, "- manual_test kind: %s\n", t.ManualTest.Kind)
	}
	if cmd := strings.TrimSpace(t.ManualTest.Command); cmd != "" {
		fmt.Fprintf(&b, "- Start it first with: %s\n", cmd)
	}
	if url := strings.TrimSpace(t.ManualTest.HealthURL); url != "" {
		fmt.Fprintf(&b, "- Wait for health on: %s\n", url)
	}
	for _, probe := range t.ManualTest.ProbeCommands {
		if probe = strings.TrimSpace(probe); probe != "" {
			fmt.Fprintf(&b, "- Run probe: %s\n", probe)
		}
	}
	b.WriteString("- Do NOT spend another turn only reading implementation files before you have started the surface and captured at least one probe result.\n")
	b.WriteString("- Background long-running servers, bound every command, and avoid the full suite (`mise run verify`, `go test ./...`, full `npm` builds).\n")
	b.WriteString("- If the manual-testing surface itself is unrunnable, say exactly why in the final report instead of looping.\n")
	return b.String()
}

func watchdogReaskNoteVarForStep(step *Step) string {
	if isTestRunnerWatchdogStep(step) {
		return testingReaskNoteVar
	}
	return watchdogReaskNoteVar
}

func isTestRunnerWatchdogStep(step *Step) bool {
	if step == nil || step.Type != StepRunAgent {
		return false
	}
	return step.ID == testVerdictSourceStep || step.Config.Role == testRunnerRole
}

func watchdogHangExhaustionResolution(t TaskInfo, step *Step, attempts int, openPROnUnrunnableGate bool) (status taskstatus.Status, reason string, terminalState ExecState) {
	if t.Status == taskstatus.Testing && isTestRunnerWatchdogStep(step) {
		if openPROnUnrunnableGate {
			return taskstatus.ReadyPR, "manual testing gate could not be run after auto-retries (harness/infra limitation, not a product defect) — opening PR for CI and human review", ExecCompleted
		}
		return taskstatus.HumanRequired, fmt.Sprintf("watchdog hang: run_test retry budget exhausted after %d clean re-dispatches", attempts), ExecFailed
	}
	return taskstatus.HumanRequired, fmt.Sprintf("watchdog hang: retry budget exhausted after %d clean re-dispatches", attempts), ExecFailed
}

func (e *Engine) handleWatchdogRateLimitRetry(t *TaskInfo, step *Step) bool {
	return e.handleWatchdogRecoveryRetry(watchdogRecoveryRateLimit, t, step)
}

// isWatchdogRateLimitReason gates the bounded retry policy that re-dispatches
// a parked run. Silent hangs are recovered by the same policy (including its
// fresh-session escape hatch below) even though they no longer claim the
// provider was rate-limited, so both reasons have to match here or a hung task
// parks forever with nothing to pick it back up.
func isWatchdogRateLimitReason(reason string) bool {
	return watchdogreason.IsRateLimit(reason) || watchdogreason.IsSilentHang(reason)
}

func watchdogRateLimitExhaustionResolution(t TaskInfo, _ *Step, attempts int) (status taskstatus.Status, reason string, terminalState ExecState) {
	if watchdogreason.IsSilentHang(t.StatusReason) {
		return taskstatus.Blocked, fmt.Sprintf("watchdog: zero-output startup retry budget exhausted after %d identical attempts", attempts+1), ExecFailed
	}
	return taskstatus.HumanRequired, fmt.Sprintf("watchdog: rate limit retry budget exhausted after %d clean re-dispatches", attempts), ExecFailed
}

func (e *Engine) canRetryWatchdogStop(t *TaskInfo, step *Step) bool {
	return t != nil &&
		t.Workflow != nil &&
		step != nil &&
		step.Type == StepRunAgent &&
		t.Status == taskstatus.HumanRequired &&
		watchdogreason.IsRetryableStop(t.StatusReason)
}

func (e *Engine) handleWatchdogStopRetry(t *TaskInfo, step *Step) bool {
	return e.handleWatchdogRecoveryRetry(watchdogRecoveryStop, t, step)
}

func (e *Engine) canRetryWorktreeRepair(t *TaskInfo, step *Step) bool {
	return t != nil &&
		t.Workflow != nil &&
		step != nil &&
		step.Type == StepRunAgent &&
		t.Status == taskstatus.Blocked &&
		t.Blocker.Kind == blocker.KindWorktreeRepair &&
		!t.Blocker.Exhausted
}

func (e *Engine) handleWorktreeRepairRetry(t *TaskInfo, step *Step) bool {
	return e.handleWatchdogRecoveryRetry(watchdogRecoveryWorktreeRepair, t, step)
}

func (e *Engine) handleWatchdogRecoveryRetry(kind watchdogRecoveryKind, t *TaskInfo, step *Step) bool {
	spec, ok := e.watchdogRecoverySpec(kind, t, step)
	if !ok {
		return false
	}
	if spec.preflight != nil && spec.preflight(e, t, step) {
		return true
	}
	return e.boundedRetry(t, step, spec.policy)
}

func (e *Engine) watchdogRecoverySpec(kind watchdogRecoveryKind, t *TaskInfo, step *Step) (watchdogRecoverySpec, bool) {
	builder, ok := watchdogRecoverySpecBuilders[kind]
	if !ok {
		return watchdogRecoverySpec{}, false
	}
	return builder(e, t, step)
}

func watchdogRunAgentStatusApplies(match func(string) bool) func(*Engine, *TaskInfo, *Step) bool {
	return func(_ *Engine, t *TaskInfo, step *Step) bool {
		return t != nil &&
			t.Workflow != nil &&
			step != nil &&
			step.Type == StepRunAgent &&
			match(t.StatusReason)
	}
}

func (e *Engine) watchdogHangRecoverySpec() watchdogRecoverySpec {
	return watchdogRecoverySpec{
		preflight: func(e *Engine, t *TaskInfo, step *Step) bool {
			return e.handleWatchdogHangReadyPR(t, step)
		},
		policy: boundedRetryPolicy{
			name:       "watchdog-hang",
			applies:    watchdogRunAgentStatusApplies(isWatchdogHangReason),
			busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
			counterKey: watchdogHangRetryKey,
			max:        maxWatchdogHangRetries,
			onArm: func(e *Engine, t *TaskInfo, step *Step, attempt int) {
				armWatchdogCleanRetry(t, step)
				t.Workflow.SetVar(watchdogReaskNoteVarForStep(step), buildWatchdogReaskNoteForStep(e.withManualTestConfig(*t), step, attempt))
			},
			onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
				return e.tasks.UpdateTaskStatus(t.ID, t.Status, "")
			},
			onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
				targetStatus, reason, terminalState := watchdogHangExhaustionResolution(*t, step, attempts, e.openPROnUnrunnableGate.Load())
				now := time.Now().UTC()
				t.Workflow.State = terminalState
				t.Workflow.CompletedAt = &now
				t.Workflow.CurrentStep = ""
				if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
					e.logger.Error("workflow.watchdog-hang.persist", "task_id", t.ID, "step", step.ID, "err", err)
				}
				if err := e.tasks.UpdateTaskStatus(t.ID, targetStatus, reason); err != nil {
					e.logger.Error("workflow.watchdog-hang.escalate", "task_id", t.ID, "step", step.ID, "err", err)
					return
				}
				if targetStatus == taskstatus.ReadyPR {
					e.logger.Warn("workflow.watchdog-hang.exhausted.open-pr", "task_id", t.ID, "step", step.ID, "attempts", attempts)
					e.fireComplete(&CompletionInfo{TaskID: t.ID, WorkflowID: t.Workflow.WorkflowID, Variables: t.Workflow.Variables})
					return
				}
				e.logger.Warn("workflow.watchdog-hang.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
			},
		},
	}
}

func (e *Engine) watchdogRewardHackingRecoverySpec(t *TaskInfo, step *Step) (watchdogRecoverySpec, bool) {
	profile, ok := rewardHackingRetryProfile(t, step)
	if !ok {
		return watchdogRecoverySpec{}, false
	}
	return watchdogRecoverySpec{
		policy: boundedRetryPolicy{
			name:       "watchdog-reward-hacking",
			applies:    watchdogRunAgentStatusApplies(isWatchdogRewardHackingReason),
			busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
			counterKey: watchdogRewardHackingRetryKey,
			max:        profile.max,
			onArm: func(_ *Engine, t *TaskInfo, step *Step, attempt int) {
				armWatchdogCleanRetry(t, step)
				t.Workflow.SetVar(watchdogReaskNoteVar, profile.note(attempt))
			},
			onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
				return e.tasks.UpdateTaskStatus(t.ID, t.Status, "")
			},
			onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
				reason := profile.exhaustedReason(attempts)
				t.Workflow.State = ExecFailed
				if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
					e.logger.Error("workflow.watchdog-reward-hacking.persist", "task_id", t.ID, "step", step.ID, "err", err)
				}
				if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.HumanRequired, reason); err != nil {
					e.logger.Error("workflow.watchdog-reward-hacking.escalate", "task_id", t.ID, "step", step.ID, "err", err)
					return
				}
				e.logger.Warn("workflow.watchdog-reward-hacking.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
			},
		},
	}, true
}

func (e *Engine) watchdogRateLimitRecoverySpec() watchdogRecoverySpec {
	return watchdogRecoverySpec{
		policy: boundedRetryPolicy{
			name:       "watchdog-rate-limit",
			applies:    watchdogRunAgentStatusApplies(isWatchdogRateLimitReason),
			counterKey: watchdogRateLimitRetryKey,
			max:        maxWatchdogRateLimitRetries,
			onArmed: func(e *Engine, t *TaskInfo, _ *Step, _ int) error {
				cleared, err := e.tasks.ClearTaskStatusReasonIf(t.ID, t.Status, t.StatusReason)
				if err != nil {
					return err
				}
				if !cleared {
					return errRetryArmingSuperseded
				}
				t.StatusReason = ""
				return nil
			},
			onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
				retryKey := watchdogRateLimitRetryKey(step.ID)
				freshKey := watchdogZeroOutputFreshRetryKey(step.ID)
				if watchdogreason.IsSilentHang(t.StatusReason) {
					sinceKey := watchdogSilentHangSinceKey(step.ID)
					since, err := time.Parse(time.RFC3339, t.Workflow.Variables[sinceKey])
					if err != nil {
						since = time.Now().UTC()
					}
					if time.Since(since) < maxSilentHangWait {
						t.Workflow.StartedAt = time.Now().UTC()
						t.Workflow.SetVar(freshKey, "1")
						t.Workflow.SetVar(retryKey, "0")
						t.Workflow.SetVar(sinceKey, since.Format(time.RFC3339))
						if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
							e.logger.Error("workflow.watchdog-rate-limit.persist", "task_id", t.ID, "step", step.ID, "err", err)
							return
						}
						e.logger.Warn("workflow.watchdog-rate-limit.fresh-session-recovery", "task_id", t.ID, "step", step.ID, "resume_attempts", attempts+1, "waiting_since", since)
						return
					}
				}
				targetStatus, reason, terminalState := watchdogRateLimitExhaustionResolution(*t, step, attempts)
				t.Workflow.State = terminalState
				if watchdogreason.IsSilentHang(t.StatusReason) {
					t.Workflow.StartedAt = time.Now().UTC()
				}
				if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
					e.logger.Error("workflow.watchdog-rate-limit.persist", "task_id", t.ID, "step", step.ID, "err", err)
				}
				var escalateErr error
				if targetStatus == taskstatus.Blocked {
					escalateErr = e.tasks.UpdateTaskBlocker(t.ID, targetStatus, reason, blocker.State{Kind: blocker.KindWatchdogRateLimitExhausted, Actor: blocker.ActorWorkflow, Exhausted: true})
				} else {
					escalateErr = e.tasks.UpdateTaskStatus(t.ID, targetStatus, reason)
				}
				if escalateErr != nil {
					e.logger.Error("workflow.watchdog-rate-limit.escalate", "task_id", t.ID, "step", step.ID, "err", escalateErr)
					return
				}
				e.logger.Warn("workflow.watchdog-rate-limit.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts, "status", targetStatus)
			},
		},
	}
}

func (e *Engine) watchdogStopRecoverySpec() watchdogRecoverySpec {
	return watchdogRecoverySpec{
		policy: boundedRetryPolicy{
			name:       "watchdog-stop",
			applies:    func(e *Engine, t *TaskInfo, step *Step) bool { return e.canRetryWatchdogStop(t, step) },
			busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
			counterKey: watchdogStopRetryKey,
			max:        maxWatchdogStopRetries,
			onArm: func(_ *Engine, t *TaskInfo, step *Step, attempt int) {
				armWatchdogCleanRetry(t, step)
				t.Workflow.SetVar(watchdogReaskNoteVarForStep(step), buildWatchdogStopReaskNote(t.StatusReason, attempt))
			},
			onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
				if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.InProgress, ""); err != nil {
					return err
				}
				t.Status = taskstatus.InProgress
				t.StatusReason = ""
				return nil
			},
			onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
				reason := fmt.Sprintf("watchdog stop: retry budget exhausted after %d clean re-dispatches", attempts)
				t.Workflow.State = ExecFailed
				if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
					e.logger.Error("workflow.watchdog-stop.persist", "task_id", t.ID, "step", step.ID, "err", err)
				}
				if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.HumanRequired, reason); err != nil {
					e.logger.Error("workflow.watchdog-stop.escalate", "task_id", t.ID, "step", step.ID, "err", err)
					return
				}
				e.logger.Warn("workflow.watchdog-stop.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
			},
		},
	}
}

func (e *Engine) worktreeRepairRecoverySpec() watchdogRecoverySpec {
	return watchdogRecoverySpec{
		policy: boundedRetryPolicy{
			name:       "worktree-repair",
			applies:    func(e *Engine, t *TaskInfo, step *Step) bool { return e.canRetryWorktreeRepair(t, step) },
			busy:       func(e *Engine, t *TaskInfo, step *Step) bool { return e.hasTrackedAgentForTaskStep(t.ID, step.ID) },
			counterKey: worktreeRepairRetryKey,
			max:        maxWorktreeRepairRetries,
			onArmed: func(e *Engine, t *TaskInfo, step *Step, attempt int) error {
				if err := e.tasks.UpdateTaskBlocker(t.ID, taskstatus.InProgress, "", blocker.State{}); err != nil {
					return err
				}
				t.Status = taskstatus.InProgress
				t.StatusReason = ""
				t.Blocker = blocker.State{}
				return nil
			},
			onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
				exhausted := t.Blocker
				exhausted.Exhausted = true
				reason := fmt.Sprintf("worktree repair: retry budget exhausted after %d attempts — manual repair required", attempts)
				if err := e.tasks.UpdateTaskBlocker(t.ID, taskstatus.Blocked, reason, exhausted); err != nil {
					e.logger.Error("workflow.worktree-repair.escalate", "task_id", t.ID, "step", step.ID, "err", err)
					return
				}
				e.logger.Warn("workflow.worktree-repair.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
			},
		},
	}
}

func armWatchdogCleanRetry(t *TaskInfo, step *Step) {
	cleanRef := t.Workflow.Variables[tamperBaselineVar(step.ID)]
	if cleanRef == "" {
		cleanRef = "HEAD"
	}
	t.Workflow.SetVar(watchdogHangCleanRetryKey(step.ID), cleanRef)
}

func buildWatchdogStopReaskNote(reason string, attempt int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Your previous agent run was STOPPED by the watchdog for loop-like behavior — attempt %d of %d.\n\n", attempt, maxWatchdogStopRetries)
	if reason = strings.TrimSpace(reason); reason != "" {
		b.WriteString("Previous watchdog reason: ")
		b.WriteString(reason)
		b.WriteString("\n\n")
	}
	b.WriteString("To make forward progress this time:\n")
	b.WriteString("- Do NOT repeat the same failing command or read-only investigation loop.\n")
	b.WriteString("- Inspect the latest concrete error/output, then change code or narrow the command before retrying.\n")
	b.WriteString("- If a deterministic check is failing, run only that narrow check and fix the root cause.\n")
	b.WriteString("- If a human genuinely must decide, stop and mark the task human-required with the exact blocker.")
	return b.String()
}

func (e *Engine) handleTransientFetchRetry(t *TaskInfo, step *Step) bool {
	return e.boundedRetry(t, step, boundedRetryPolicy{
		name: "transient-fetch",
		applies: func(_ *Engine, t *TaskInfo, step *Step) bool {
			return t != nil && t.Workflow != nil && step != nil && step.Type == StepRunAgent && isTransientFetchReason(t.StatusReason)
		},
		counterKey: transientFetchRetryKey,
		max:        maxTransientFetchRetries,
		onExhausted: func(e *Engine, t *TaskInfo, step *Step, attempts int) {
			reason := fmt.Sprintf("agent start blocked: transient network retry budget exhausted after %d attempts reconciling worktree with remote", attempts)
			if err := e.tasks.UpdateTaskStatus(t.ID, taskstatus.HumanRequired, reason); err != nil {
				e.logger.Error("workflow.transient-fetch.escalate", "task_id", t.ID, "step", step.ID, "err", err)
				return
			}
			e.logger.Warn("workflow.transient-fetch.exhausted", "task_id", t.ID, "step", step.ID, "attempts", attempts)
		},
	})
}

func (e *Engine) clearTransientFetchRetry(taskID string, wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	retryKey := transientFetchRetryKey(stepID)
	if _, ok := wf.Variables[retryKey]; !ok {
		return
	}
	delete(wf.Variables, retryKey)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.transient-fetch.clear", "task_id", taskID, "step", stepID, "err", err)
	}
}

func (e *Engine) clearWatchdogReaskNote(taskID string, wf *Execution) {
	if wf == nil || wf.Variables == nil {
		return
	}
	if _, ok := wf.Variables[watchdogReaskNoteVar]; !ok {
		return
	}
	delete(wf.Variables, watchdogReaskNoteVar)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.watchdog-hang.reask-clear", "task_id", taskID, "err", err)
	}
}

func (e *Engine) clearDeliveredWatchdogReaskNote(taskID string, step *Step, wf *Execution) {
	if step == nil || step.ID == "" || wf == nil || wf.Variables == nil {
		return
	}
	if !e.watchdogReaskDelivered(taskID, step, wf) {
		return
	}
	delete(wf.Variables, watchdogReaskDeliveredKey(step.ID))
	e.clearWatchdogReaskNote(taskID, wf)
	if err := e.tasks.SetWorkflow(taskID, wf); err != nil {
		e.logger.Error("workflow.watchdog-reask.delivery-clear", "task_id", taskID, "step", step.ID, "err", err)
	}
}

func (e *Engine) watchdogReaskDelivered(taskID string, step *Step, wf *Execution) bool {
	if workflowHasAgentRouteForStep(wf, step) || e.hasPendingAgentRouteForStep(taskID, step) {
		return true
	}
	return wf != nil && wf.Variables[watchdogReaskDeliveredKey(step.ID)] == "1"
}

func watchdogReaskDeliveredKey(stepID string) string { return "watchdog_reask_delivered." + stepID }

func clearWatchdogRetryCounters(wf *Execution, stepID string) {
	if wf == nil || wf.Variables == nil || stepID == "" {
		return
	}
	for _, key := range []string{
		watchdogRewardHackingRetryKey(stepID),
		watchdogHangRetryKey(stepID),
		watchdogStopRetryKey(stepID),
		watchdogRateLimitRetryKey(stepID),
		watchdogZeroOutputFreshRetryKey(stepID),
		watchdogSilentHangSinceKey(stepID),
	} {
		delete(wf.Variables, key)
	}
}

func watchdogHangRetryKey(stepID string) string {
	return watchdogHangRetryVarPrefix + stepID
}

func watchdogStopRetryKey(stepID string) string {
	return watchdogStopRetryVarPrefix + stepID
}

func watchdogHangCleanRetryKey(stepID string) string {
	return watchdogHangCleanRetryVarPrefix + stepID
}

func watchdogRateLimitRetryKey(stepID string) string {
	return watchdogRateLimitRetryVarPrefix + stepID
}

func watchdogZeroOutputFreshRetryKey(stepID string) string {
	return watchdogZeroOutputFreshRetryVarPrefix + stepID
}

func watchdogSilentHangAvoidKey(stepID string) string {
	return watchdogSilentHangAvoidVarPrefix + stepID
}

func watchdogSilentHangSinceKey(stepID string) string {
	return watchdogSilentHangSinceVarPrefix + stepID
}

// markSilentHangProvider records the provider of a run the watchdog killed for
// producing no output, so the next dispatch of the same step routes around it.
// Called before the retry gate clears the status reason, which is the only
// evidence at this point that the stop was a silent hang rather than a real
// rate limit.
func (e *Engine) markSilentHangProvider(t *TaskInfo, step *Step, agentID string) {
	if t == nil || t.Workflow == nil || step == nil || !watchdogreason.IsSilentHang(t.StatusReason) {
		return
	}
	prov := runProviderByAgentID(t.AgentRuns, agentID)
	if prov == "" {
		return
	}
	t.Workflow.SetVar(watchdogSilentHangAvoidKey(step.ID), prov)
	if err := e.tasks.SetWorkflow(t.ID, t.Workflow); err != nil {
		e.logger.Error("workflow.silent-hang.avoid-persist", "task_id", t.ID, "step", step.ID, "err", err)
	}
}

func runProviderByAgentID(runs []AgentRunInfo, agentID string) string {
	if agentID == "" {
		return ""
	}
	for i := range slices.Backward(runs) {
		if runs[i].AgentID == agentID {
			return normalizeExplicitWorkflowProvider(runs[i].Provider)
		}
	}
	return ""
}

func worktreeRepairRetryKey(stepID string) string {
	return worktreeRepairRetryVarPrefix + stepID
}

func transientFetchRetryKey(stepID string) string {
	return transientFetchRetryVarPrefix + stepID
}

func parseWorkflowInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
