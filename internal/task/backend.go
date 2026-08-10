package task

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
)

// ErrCreateIDCollision is CreateBy's signal that the candidate ID is already
// taken and the caller should mint a fresh one and retry — the sentinel a
// Persistence implementation maps its own collision error to (taskdb.Persistence
// wraps taskdb.ErrIDCollision; the file backend never produces it, since
// Store.createNewTask retries collisions internally and only surfaces an
// already-exhausted, non-transient error).
var ErrCreateIDCollision = errors.New("task: create id collision")

// maxTaskIDMintAttempts bounds collision retry when minting a new task ID
// against a Persistence backend — mirrors Store's own maxTaskIDAttempts.
const maxTaskIDMintAttempts = 16

// newTaskIDFn is overridden in tests, the same seam Store.newTaskID is.
var newTaskIDFn = func() string { return uuid.NewString()[:8] }

// buildNewTask computes the initial state for a task minted from title/body/mode plus init overrides — the same construction Store.CreateFull does for the file backend (mode validation, applyCreateInit, the explicit-clear/blocker/sandbox checks, tamper flagging, and the sidecar-string fields init sets) — factored out so a Persistence-backed create only needs to mint an ID and write the result rather than duplicate that construction.
func buildNewTask(title, body, mode string, init Update) (Task, error) {
	if mode == "" {
		mode = AgentModeHeadless
	}
	if _, err := ValidateMintableAgentMode(mode); err != nil {
		return Task{}, err
	}
	now := time.Now().UTC()
	t := Task{
		Slug: Slugify(title), Title: title, Status: StatusTodo, Generation: 1,
		AgentMode: mode, Attachments: []Attachment{}, CreatedAt: now, UpdatedAt: now,
		StatusChangedAt: now, Body: body,
	}
	applyCreateInit(&t, init, now)
	if err := validateExplicitClear(init); err != nil {
		return Task{}, err
	}
	if err := blocker.ValidateStatus(string(t.Status), t.Blocker); err != nil {
		return Task{}, err
	}
	if err := normalizeSandboxEscapeHatch(&t); err != nil {
		return Task{}, err
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.Blocker)
	if err := applySidecarUpdateFields(&t, init); err != nil {
		return Task{}, err
	}
	return t, nil
}

// mintAndCreateBy assigns t a fresh ID, retrying on the vanishingly rare
// collision the same way Store.createNewTask's loop does, and persists it —
// the Persistence-backed equivalent of Store.CreateFull's ID-minting half.
func mintAndCreateBy(persist Persistence, t Task, actor string) (Task, error) {
	var lastErr error
	for range maxTaskIDMintAttempts {
		t.ID = newTaskIDFn()
		saved, err := persist.CreateBy(t, actor)
		if err == nil {
			return saved, nil
		}
		if !errors.Is(err, ErrCreateIDCollision) {
			return Task{}, err
		}
		lastErr = err
	}
	return Task{}, fmt.Errorf("mint task id: %d attempts exhausted, last error: %w", maxTaskIDMintAttempts, lastErr)
}

// LegacyActor marks a history entry written by a mutation path not yet migrated to name a real actor — see the follow-up issue linked from #3268. Never blank: a blank actor is indistinguishable from "no filter" when History is queried back by actor, so it reads as a mystery rather than a known, greppable gap.
const LegacyActor = "task.manager.legacy"

// Persistence is the storage primitive Manager's task-CRUD methods run against, selected once at construction: the file-backed *Store (always available, today's default) or a database-backed adapter. It does not cover everything Store does — Comments, the ten planning/review sidecar fields plus PlanDrafts stay on Store's existing sidecar sub-stores for now (folded into PutBy/PutFnBy's Task argument on read and write, same shape either backend uses), and trash-generation history (ListTrash/PruneTrash/DeleteTrashedGeneration/PruneAllTrash) and the leader-follower mirror's direct sidecar-substore writes stay on the file Store Manager always also holds. See the follow-up issue for moving those onto the configured backend too.
//
// Every mutation takes actor and refuses a blank one (Manager enforces this, not the implementations) — the same "refuse rather than record anonymous" rule status transitions already apply via TransitionIntent.
type Persistence interface {
	Get(id string) (Task, error)
	List() ([]Task, error)
	// PutBy stores t verbatim (create or upsert by ID), recording actor and, for an update, which fields changed.
	PutBy(t Task, actor string, changed []string) (Task, error)
	// PutFnBy atomically reads the current task, lets fn compute the replacement and the fields it changed, and writes both back — for read-modify-write callers that would otherwise race a concurrent mutation of the same id between their read and their write.
	PutFnBy(id, actor string, fn func(cur Task) (next Task, changed []string, err error)) (Task, error)
	// CreateBy inserts t as a brand-new task, refusing to silently overwrite an existing row the way PutBy's upsert would — the guarantee Create needs that Update/Put do not. A file-backed implementation mints its own ID, ignoring t.ID; a database-backed one uses t.ID as the candidate and errors on collision so the caller can mint a new one and retry.
	CreateBy(t Task, actor string) (Task, error)
	// UpdateFieldsBy atomically reads the current task, lets compute produce the Update to apply, and persists the result with the full ApplyUpdate bookkeeping (generation, timestamps, tamper flag, sidecar fields) — a separate method from PutFnBy because turning an Update into a persisted write needs backend-specific handling PutFnBy's generic Task-in-Task-out shape can't express safely (the file backend's sidecar files, in particular, are only written by Store.UpdateWithPrev, not by a plain whole-task overwrite). Returns the task's status before compute's Update was applied, for a caller's hook-firing decision.
	UpdateFieldsBy(id, actor string, compute func(cur Task) (Update, error)) (saved Task, prevStatus Status, err error)
	DeleteBy(id, actor string) error
	RestoreBy(id, actor string) error
}

// applyUpdate computes the next Task state for u applied to cur, including the generation/timestamp/tamper-flag bookkeeping every field update already gets — the same computation Store.UpdateWithPrev does for the file backend, factored out so a Persistence implementation only needs to persist the result rather than duplicate that bookkeeping. changed is the non-nil field list recorded in history.
func ApplyUpdate(cur Task, u Update) (next Task, changed []string, err error) {
	t := cur
	prevStatus := t.Status
	if err := applyUpdateFields(&t, u); err != nil {
		return Task{}, nil, err
	}
	now := time.Now().UTC()
	if prevStatus == StatusHumanRequired && t.Status != StatusHumanRequired && u.TestingCycleStartedAt == nil {
		t.TestingCycleStartedAt = &now
	}
	statusChangedBackfill := statusChangedAtBackfill(t, now)
	t.Generation++
	t.UpdatedAt = now
	if t.Status != prevStatus {
		t.StatusChangedAt = now
	} else if t.StatusChangedAt.IsZero() {
		t.StatusChangedAt = statusChangedBackfill
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.Blocker)
	if err := applySidecarUpdateFields(&t, u); err != nil {
		return Task{}, nil, err
	}
	return t, changedFields(u), nil
}

// applySidecarUpdateFields is writeSidecars' field-assignment half without the file write: a Persistence implementation persists the ten planning/review strings as part of the whole Task it stores (folded into the doc for the file backend via its own sidecar files, or into taskdb's sidecar rows via taskdb.SidecarsFromTask) rather than as a separate side-effecting write here. Returns an error only for PlanDraftWrite: the file backend's own PlanDraftStore.Write validates the name before it becomes part of a filename, and a Persistence implementation that skips PlanDraftStore entirely (taskdb writes straight to a column) would otherwise accept a name the file backend rejects, making a single Update's validity depend on which backend happens to be configured.
func applySidecarUpdateFields(t *Task, u Update) error {
	if u.Plan != nil {
		t.Plan = *u.Plan
	}
	if u.PlanContract != nil {
		t.PlanContract = *u.PlanContract
	}
	if u.PlanCritique != nil {
		t.PlanCritique = *u.PlanCritique
	}
	if u.PlanResearch != nil {
		t.PlanResearch = *u.PlanResearch
	}
	if u.PlanDecisions != nil {
		t.PlanDecisions = *u.PlanDecisions
	}
	if u.PlanBrief != nil {
		t.PlanBrief = *u.PlanBrief
	}
	if u.CodeReview != nil {
		t.CodeReview = *u.CodeReview
	}
	if u.CurrentTestFailures != nil {
		t.CurrentTestFailures = *u.CurrentTestFailures
	}
	if u.AcceptanceLedger != nil {
		t.AcceptanceLedger = *u.AcceptanceLedger
	}
	if u.SpecDecision != nil {
		t.SpecDecision = *u.SpecDecision
	}
	if u.PlanDraftWrite != nil {
		if err := ValidatePlanDraftName(u.PlanDraftWrite.Name); err != nil {
			return err
		}
		if t.PlanDrafts == nil {
			t.PlanDrafts = make(map[string]string)
		}
		t.PlanDrafts[u.PlanDraftWrite.Name] = u.PlanDraftWrite.Content
	}
	return nil
}

// changedFields names u's non-nil fields for the history entry, using the same wire-facing names UpdateFromMap accepts, so a query against history and a mutation through the API describe a field the same way.
func changedFields(u Update) []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(u.Title != nil, "title")
	add(u.Slug != nil, "slug")
	add(u.Status != nil, "status")
	add(u.StatusReason != nil, "status_reason")
	add(u.ClearStatusReason != nil, "clear_status_reason")
	add(u.Escalation != nil, "escalation")
	add(u.AutonomyOutcome != nil, "autonomy_outcome")
	add(u.Blocker != nil, "blocker")
	add(u.ClearBlocker != nil, "clear_blocker")
	add(u.ClearWorkflow != nil, "clear_workflow")
	add(u.BlockedByIssue != nil, "blocked_by_issue")
	add(u.UmbrellaIssue != nil, "umbrella_issue")
	add(u.DependsOn != nil, "depends_on")
	add(u.DependsOnConditions != nil, "depends_on_conditions")
	add(u.AgentMode != nil, "agent_mode")
	add(u.TaskType != nil, "task_type")
	add(u.Body != nil, "body")
	add(u.Tags != nil, "tags")
	add(u.ProjectID != nil, "project_id")
	add(u.Branch != nil, "branch")
	add(u.WorktreeDir != nil, "worktree_dir")
	add(u.HandoffSourceProvider != nil, "handoff_source_provider")
	add(u.PRNumber != nil, "pr_number")
	add(u.Issue != nil, "issue")
	add(u.RefIssue != nil, "ref_issue")
	add(u.Reviewed != nil, "reviewed")
	add(u.RunRole != nil, "run_role")
	add(u.SupervisorSteer != nil, "supervisor_steer")
	add(u.ReviewPhase != nil, "review_phase")
	add(u.ReviewedHeadSHA != nil, "reviewed_head_sha")
	add(u.ReviewedHeadAttempts != nil, "reviewed_head_attempts")
	add(u.ReconcileFailures != nil, "reconcile_failures")
	add(u.PRPhase != nil, "pr_phase")
	add(u.Priority != nil, "priority")
	add(u.DueDate != nil, "due_date")
	add(u.Workflow != nil, "workflow")
	add(u.Plan != nil, "plan")
	add(u.PlanContract != nil, "plan_contract")
	add(u.PlanCritique != nil, "plan_critique")
	add(u.PlanResearch != nil, "plan_research")
	add(u.PlanDecisions != nil, "plan_decisions")
	add(u.PlanBrief != nil, "plan_brief")
	add(u.CodeReview != nil, "code_review")
	add(u.CurrentTestFailures != nil, "current_test_failures")
	add(u.AcceptanceLedger != nil, "acceptance_ledger")
	add(u.SpecDecision != nil, "spec_decision")
	add(u.PlanDraftWrite != nil, "plan_draft")
	add(u.CodeReviewVerdict != nil, "code_review_verdict")
	add(u.MaxTurns != nil, "max_turns")
	add(u.ForkSubagent != nil, "fork_subagent")
	add(u.Sandbox != nil, "sandbox")
	add(u.SandboxOffReason != nil, "sandbox_off_reason")
	add(u.ReasoningEffort != nil, "reasoning_effort")
	add(u.Outcome != nil, "outcome")
	add(u.MergeCommit != nil, "merge_commit")
	add(u.TestingCycleStartedAt != nil, "testing_cycle_started_at")
	add(u.Attachments != nil, "attachments")
	add(u.EffectLog != nil, "effect_log")
	return out
}

// applyAddRun computes cur with run appended to AgentRuns and, when status is
// non-nil, the same atomic status-plus-run transition Store.addRun applies
// for the file backend — status-reason/blocker/escalation/outcome reset on a
// real status change, ClosedAt stamped or cleared on entering or leaving a
// terminal status, StatusChangedAt backfilled otherwise.
func applyAddRun(cur Task, run AgentRun, status *Status) (Task, error) {
	t := cur
	now := time.Now().UTC()
	if status != nil {
		oldStatus := t.Status
		if *status == StatusHumanRequired && oldStatus != StatusHumanRequired {
			return Task{}, fmt.Errorf("add run with status: transition to human-required requires typed escalation evidence")
		}
		t.Status = *status
		if oldStatus != t.Status {
			t.StatusChangedAt = now
			t.StatusReason = ""
			t.Blocker = blocker.State{}
			t.Escalation = autonomy.EscalationReason{}
			t.AutonomyOutcome = ""
		} else {
			backfillStatusChangedAt(&t, now)
		}
		wasTerminal := IsTerminalStatus(oldStatus)
		isTerminal := IsTerminalStatus(t.Status)
		if !wasTerminal && isTerminal {
			t.ClosedAt = &now
		} else if wasTerminal && !isTerminal {
			t.ClosedAt = nil
		}
	} else {
		backfillStatusChangedAt(&t, now)
	}
	t.AgentRuns = append(t.AgentRuns, run)
	t.UpdatedAt = now
	return t, nil
}

// applyRunUpdate computes cur with patch applied to the AgentRun matching
// agentID, the same lookup-and-patch UpdateRun applies for the file backend.
// Returns an error if no run in cur matches agentID.
func applyRunUpdate(cur Task, agentID string, patch RunPatch) (Task, error) {
	t := cur
	found := false
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID != agentID {
			continue
		}
		found = true
		applyRunPatch(&t.AgentRuns[i], patch)
		break
	}
	if !found {
		return Task{}, fmt.Errorf("agent run %s not found for task %s", agentID, t.ID)
	}
	now := time.Now().UTC()
	backfillStatusChangedAt(&t, now)
	t.UpdatedAt = now
	return t, nil
}

// sidecarUpdateFromTask builds the Update fileBackend.CreateBy passes to
// Store.CreatePrebuilt so it also writes t's already-populated sidecar
// fields to their own files — pointers only for the non-empty ones, since a
// freshly minted task has nothing to explicitly clear.
func sidecarUpdateFromTask(t Task) Update {
	var u Update
	set := func(field **string, v string) {
		if v != "" {
			*field = &v
		}
	}
	set(&u.Plan, t.Plan)
	set(&u.PlanContract, t.PlanContract)
	set(&u.PlanCritique, t.PlanCritique)
	set(&u.PlanResearch, t.PlanResearch)
	set(&u.PlanDecisions, t.PlanDecisions)
	set(&u.PlanBrief, t.PlanBrief)
	set(&u.CodeReview, t.CodeReview)
	set(&u.CurrentTestFailures, t.CurrentTestFailures)
	set(&u.AcceptanceLedger, t.AcceptanceLedger)
	set(&u.SpecDecision, t.SpecDecision)
	return u
}
