package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/clock"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/prompteval"
	"github.com/Automaat/sybra/internal/reconcile"

	"github.com/Automaat/sybra/internal/taskstatus"
)

const (
	maxSyncSteps   = 100 // depth limit for synchronous step chains
	maxStepHistory = 50  // max step records kept per execution
	shellTimeout   = 30 * time.Second
	// Deliberately generous: effect leases have no heartbeat yet, so they must
	// outlive the longest expected synchronous step execution to avoid false
	// reclaims mid-step.
	defaultEffectLeaseTTL = 30 * time.Minute
)

// TaskInfo is the subset of task data the engine needs.
type TaskInfo struct {
	ID    string
	Title string
	// Generation is the reducer-visible monotonic task version the effect-id
	// scheme keys on until a dedicated persisted task-generation counter lands.
	Generation   int64
	Status       taskstatus.Status
	StatusReason string
	Blocker      blocker.State
	Role         string
	Tags         []string
	AgentMode    string
	// Priority mirrors task.Priority ("", "low", "medium", "high", "urgent").
	// Kept as a plain string, not task.Priority, since internal/workflow must
	// stay decoupled from internal/task (see dispatchorder's package doc) —
	// an app-wired SetDispatchComparator converts it back when building an
	// agentqueue.Item to reuse agentqueue.Less for ResumeStalled's ordering.
	Priority              string
	ProjectID             string
	ProjectType           string
	NodeOverride          string
	HandoffSourceProvider string
	PRNumber              int
	Branch                string
	Body                  string
	Plan                  string
	PlanContract          string
	PlanCritique          string
	PlanResearch          string
	PlanDecisions         string
	PlanBrief             string
	CodeReview            string
	CurrentTestFailures   string
	AcceptanceLedger      string
	SpecDecision          string
	CodeReviewVerdict     string
	Attachments           []AttachmentInfo
	// PlanDrafts holds raw per-provider plans during dual-/N-provider planning.
	// Keys are parallel child step IDs (e.g. "plan_claude", "plan_codex").
	PlanDrafts map[string]string
	Issue      string
	Reviewed   bool
	Workflow   *Execution
	// AgentRuns is the minimal per-task agent-run history the engine needs.
	// route_test_result counts prior test-runner runs; provider=cross uses
	// code-authoring run providers when a previous workflow wrote the code.
	AgentRuns []AgentRunInfo
	// TestingCycleStartedAt is the start of the current testing cycle, set when
	// a human re-dispatches from human-required. Nil means no re-dispatch has
	// occurred; route_test_result counts all test-runner runs in that case.
	TestingCycleStartedAt *time.Time
	// ManualTest is repo/project-declared guidance for black-box testing.
	ManualTest ManualTestInfo
}

// AttachmentInfo is the workflow-visible subset of task attachment metadata.
type AttachmentInfo struct {
	ID          string
	FileName    string
	ContentType string
	SizeBytes   int64
	Path        string
}

// ManualTestInfo describes the runnable surface a test-runner should exercise.
type ManualTestInfo struct {
	Kind          string
	Command       string
	HealthURL     string
	ProbeCommands []string
}

// AgentRunInfo is the engine-visible subset of a task's agent run.
type AgentRunInfo struct {
	AgentID                string
	Role                   string
	Provider               string
	Mode                   string
	State                  string
	Outcome                string
	RequestedSkill         string
	SkillExecutionMode     string
	SkillConformance       string
	StartedAt              time.Time
	ProtocolViolation      string
	TestOutcome            string
	TestFailureFingerprint string
	HeadSHA                string
	FinalCommitSource      string
	// SubagentCallCount is the number of distinct forked-subagent calls the
	// run made (task.AgentRun.SubagentCallCount). Used to tell a genuine
	// no-op run apart from one that delegated to a background subagent and
	// ended before that delegation produced any commits.
	SubagentCallCount int
	// TurnCount is zero when the provider child emitted nothing, marking a run
	// that never saw its instructions rather than one that defied them.
	TurnCount int
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	ListTasks() ([]TaskInfo, error)
	UpdateTaskStatus(id string, status taskstatus.Status, reason string) error
	// ClearTaskStatusReasonIf atomically clears a status reason only when the
	// task still has the exact status and reason the caller observed. It keeps a
	// stale retry cleanup from erasing a newer failure or operator decision.
	ClearTaskStatusReasonIf(id string, expectedStatus taskstatus.Status, expectedReason string) (bool, error)
	// ClearTaskStatusReasonAndSetWorkflowIf folds that compare-and-swap clear
	// and the Workflow write into one store call, so an armed retry never
	// persists its incremented counter without also dropping the marker that
	// armed it — and writes nothing at all when the compare fails.
	ClearTaskStatusReasonAndSetWorkflowIf(id, expectedStatus, expectedReason string, wf *Execution) (bool, error)
	UpdateTaskBlocker(id string, status taskstatus.Status, reason string, state blocker.State) error
	UpdateTaskPR(id string, prNumber int) error
	MarkTaskReviewed(id string) error
	// SetCodeReviewVerdict persists the review-role step's structured verdict
	// ("CLEAN"/"NEEDS_FIXES") separately from the CodeReview sidecar markdown,
	// so a later re-triggered workflow can read it without re-parsing text.
	SetCodeReviewVerdict(id, verdict string) error
	MarkAgentRunProtocolViolation(taskID, agentID, violation string) error
	MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error
	RecordAgentRunFinalCommit(taskID, agentID, headSHA, source string) error
	// MarkAgentRunIncomplete downgrades a clean-exit code-author run that
	// produced no commits. Named rather than taking an outcome string because
	// internal/workflow cannot import internal/task (cycle via
	// internal/agent), so the vocabulary stays on the far side of the adapter.
	MarkAgentRunIncomplete(taskID, agentID string) error
	AppendTaskBody(id, content string) error
	// ReplaceTaskBody overwrites the task's full body. Used by the test-route
	// step to archive/strip a stale "## Test Failures" section before
	// appending a new one, so at most one such heading is ever live in the
	// body (see stripTestFailuresSections).
	ReplaceTaskBody(id, body string) error
	SetWorkflow(id string, wf *Execution) error
	// SetStatusAndWorkflow persists Status/StatusReason and Workflow in a
	// single atomic write. Use this instead of a paired
	// UpdateTaskStatus+SetWorkflow call whenever both change together — the
	// two-call sequence leaves a crash window where a restart between them
	// can land a terminal status with a still-running workflow (or vice
	// versa). reason == "" leaves the task's current StatusReason
	// untouched.
	SetStatusAndWorkflow(id, status, reason string, wf *Execution) error
	// SetEscalationAndWorkflow is the typed human-required/quarantine form of
	// SetStatusAndWorkflow. The reason text is display-only; escalation and
	// outcome are the policy/audit authority.
	SetEscalationAndWorkflow(id, status, reason string, escalation autonomy.EscalationReason, outcome autonomy.Outcome, wf *Execution) error
	// SetBlockerAndWorkflow is SetStatusAndWorkflow's counterpart for callers
	// escalating to a blocked status with a workflow-owned blocker.State —
	// same single-write atomicity guarantee, blocker included.
	SetBlockerAndWorkflow(id, status, reason string, state blocker.State, wf *Execution) error
	// SetWorkflowIf atomically replaces the workflow only while the persisted
	// task still matches fence. It prevents a maintenance scan from overwriting
	// a newer operator/automation workflow with a stale snapshot.
	SetWorkflowIf(id string, fence WorkflowWriteFence, wf *Execution) (bool, error)
	// SetStatusAndWorkflowIf atomically applies a reconciled workflow routing
	// effect while the same workflow write fence still holds.
	SetStatusAndWorkflowIf(id string, fence WorkflowWriteFence, status taskstatus.Status, reason string, wf *Execution) (bool, error)
	ClaimWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error)
	CompleteWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error)
	ReleaseWorkflowEffect(id string, claim EffectClaim) (EffectClaimResult, error)
	// ConsumeSupervisorSteer prepends a pending watchdog headless-nudge steer to
	// prompt and clears it, so a re-dispatched (resumed) step's agent carries the
	// correction exactly once. Returns prompt unchanged when none is pending.
	ConsumeSupervisorSteer(taskID, prompt string) (string, error)
	// WriteSidecar stores content as the named sidecar for the task. Used
	// by run_agent steps that declare import_sidecar so the engine can
	// ingest the agent's output file without depending on the agent's
	// sandbox being able to write to ~/.sybra/tasks/. Recognized kinds:
	//   "plan"          — final implementation plan
	//   "plan_contract" — executable JSON plan contract
	//   "plan_critique" — plan-critic report
	//   "plan_research" — raw planning research/evidence
	//   "plan_decisions" — human decision brief
	//   "plan_brief"    — final human-facing review brief
	//   "code_review"   — staff-code-review report
	//   "current_test_failures" — latest manual-test failure report
	//   "acceptance_ledger" — bounded ledger of distinct failure repros
	//   "spec_decision" — latest acceptance-conflict escalation summary
	//   "plan_draft.<name>" — raw per-provider plan during dual-/N-provider
	//       planning; <name> is typically the parallel child step ID. The
	//       engine derives <name> from the step ID when the YAML kind is
	//       a bare "plan_draft".
	WriteSidecar(id, kind, content string) error
}

// WorkflowWriteFence identifies the exact task/workflow snapshot a
// maintenance mutation is allowed to replace.
type WorkflowWriteFence struct {
	Generation   int64
	Status       taskstatus.Status
	StatusReason string
	WorkflowID   string
	CurrentStep  string
	State        ExecState
}

// WorktreeGetter resolves the filesystem path of a task's git worktree.
// Returns (path, true) when a worktree exists for the task; ("", false) when
// none is found. Implementations may stat the path to confirm existence.
// Engine operates with a nil WorktreeGetter — verify_commits becomes a no-op.
type WorktreeGetter interface {
	GetWorktreePath(taskID string) (string, bool)
}

// PRWorktreeResolver resolves or reconstructs the implementation worktree for
// deterministic PR-tail steps. It is optional so lightweight Engine users and
// tests can keep providing a read-only WorktreeGetter.
type PRWorktreeResolver interface {
	ResolvePRWorktree(ctx context.Context, taskID string) (path string, found bool, err error)
}

// AttemptNoteAppender persists re-implementation context into a task's local
// NOTES.md scratchpad. Engine passes the already-resolved worktree path so the
// adapter can reuse the same path for both diff inspection and the append.
type AttemptNoteAppender interface {
	AppendReimplementNote(ctx context.Context, taskID, wtPath, marker, note string) error
}

// BranchSyncer proactively reconciles a task's worktree branch with the
// project's default branch. Used by the `sync_branch` step. Engine operates
// with a nil BranchSyncer — the step then records a skipped outcome and
// never blocks workflow advancement.
type BranchSyncer interface {
	SyncTaskBranch(ctx context.Context, taskID string) (result string, err error)
}

// CheckConfigGetter resolves a task's codegen and verify command sets, merged
// from repo `.sybra.yaml` and the app-level project config. Returns nil/empty
// when the task has no verify suite or codegen pass configured — the
// verify_checks/codegen_gate steps then become no-ops. Engine operates with a
// nil getter (step skips), so unit tests need not wire one.
type CheckConfigGetter interface {
	CodegenCommands(ctx context.Context, taskID string) []string
	VerifyCommands(ctx context.Context, taskID string) []string
	SetupCommands(ctx context.Context, taskID string) []string
	FocusedChecks(ctx context.Context, taskID string) []project.FocusedCheck
}

// ManualTestConfigGetter resolves repo/project-declared black-box testing hints.
// Engine operates with a nil getter — prompts then fall back to repo discovery.
type ManualTestConfigGetter interface {
	ManualTestConfig(taskID string) ManualTestInfo
}

// PRLinker inspects and updates GitHub pull request metadata for the
// `ensure_pr_closes_issue` step. Implementations wrap `gh` CLI calls.
// Engine operates with a nil PRLinker — the step becomes a no-op when
// unset, so tests don't need to wire one.
type PRLinker interface {
	// GetClosingIssues returns issue numbers the PR's body is parsed by
	// GitHub as closing, scoped to the same repo as the PR. Also returns
	// the current PR body so callers can edit it without a second fetch.
	GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error)
	// EditBody replaces the PR body.
	EditBody(repo string, prNumber int, body string) error
}

// PRReviewRequester re-requests review from prior PR commenters after a
// workflow fixes review comments and pushes updated commits.
type PRReviewRequester interface {
	RerequestReview(repo string, prNumber int) (reviewers []string, err error)
	RequestCopilotReview(ctx context.Context, repo string, prNumber int) error
}

// PRStateFetcher fetches the live state of a GitHub pull request. Used by
// `route_pr_fix_result` to re-probe the remote PR before parking a task
// human-required — a pr-fix agent that declined to push because its local
// worktree was stale/diverged may be sitting on a PR an external bot already
// fixed. Engine operates with a nil fetcher — the re-probe is then skipped
// and the agent's sentinel is trusted as-is, so tests don't need to wire one.
type PRStateFetcher interface {
	FetchPRState(repo string, number int) (github.PRState, error)
}

// PRHeadFetcher looks up a PR's live head commit SHA. Used by `push_branch`
// to verify a push landed before continuing. Engine operates with a nil
// fetcher — the step then skips verification and trusts the push exit code.
type PRHeadFetcher interface {
	FetchPRHeadSHA(ctx context.Context, repo string, number int) (string, error)
}

// PushCredentialPreflighter validates that the current process can authenticate
// to the worktree's configured push remote before push-dependent workflow
// steps spend more work or attempt a real push. Engine operates with a nil
// preflighter by falling back to project.PreflightPushCredentials.
type PushCredentialPreflighter interface {
	PreflightPushCredentials(ctx context.Context, worktreePath string) error
}

// PRCreator opens a new GitHub pull request for an already-pushed branch via
// `gh pr create`, run inside the task's worktree so gh resolves the same
// repo/fork context an interactive invocation would. Used by the `create_pr`
// step — the deterministic replacement for the create-pr agent. Engine
// operates with a nil PRCreator — the step then flips the task to
// human-required, since PR creation is mandatory to progress.
type PRCreator interface {
	CreatePR(ctx context.Context, dir string, req PRCreateRequest) (number int, headSHA string, err error)
}

// PRCloser closes an open pull request that has been superseded by a newer PR
// for the same task. Engine treats this as best-effort cleanup: a close failure
// must not roll back linking the replacement PR.
type PRCloser interface {
	ClosePR(ctx context.Context, repo string, number int, comment string) error
}

// PRFinder looks up an open PR by its head branch, backing the create_pr
// idempotency guard (a prior run may have created the PR but crashed before
// persisting pr_number). Engine operates with a nil finder — the guard is
// then skipped and create_pr always attempts a fresh push/create.
type PRFinder interface {
	FindPRForBranch(ctx context.Context, repo, head string) (number int, found bool, err error)
}

// PRAnyStateFinder resolves a PR for a head branch across open and merged
// states. Used as a stronger duplicate guard before create_pr opens a new PR:
// a previously squash-merged PR can leave the branch tip non-ancestor of base
// while the tree diff is already present on base.
type PRAnyStateFinder interface {
	FindPRForBranchAnyState(ctx context.Context, repo, head string) (number int, state string, found bool, err error)
}

// PRExistenceChecker confirms a pr_number actually resolves against a given
// repo, guarding link_pr_and_review against trusting an out-of-band pr_number
// set for the wrong repo (e.g. an agent that created a PR in its own fork
// instead of upstream). Engine operates with a nil checker — the guard is
// then skipped and task.pr_number is trusted as before.
type PRExistenceChecker interface {
	PRExists(ctx context.Context, repo string, number int) (bool, error)
}

// PRCreateRequest describes a new pull request to open for an
// already-pushed branch.
type PRCreateRequest struct {
	// Repo is the base repo the PR is opened against, "owner/name".
	Repo string
	// Head is the `gh pr create --head` value: a bare branch name, or
	// "fork-owner:branch" when the branch lives on a fork.
	Head  string
	Draft bool
	Title string
	Body  string
}

// PRContentGenerator drafts the PR title/body for the `create_pr` step via a
// single cheap LLM job — the only LLM involvement left in the create_pr
// tail now that push/create are deterministic Go. Engine operates with a nil
// generator — the step then falls back to a templated title/body derived
// from the task itself, so tests and misconfigured deployments still work.
type PRContentGenerator interface {
	GeneratePRContent(ctx context.Context, taskTitle, taskBody string, commitSubjects []string) (title, body string, err error)
}

// TaskClassifier runs the deterministic Go triage classifier directly against
// a task and applies its verdict. Used by the `classify_task` step, which
// replaced a run_agent step that wrapped a full Sonnet agent (invoking the
// /sybra-triage skill) around this same classifier — a second LLM call for
// no benefit. Engine operates with a nil classifier — the step then flips
// the task to human-required, since triage is mandatory to route the task.
type TaskClassifier interface {
	ClassifyTask(ctx context.Context, taskID string) error
}

// ArtifactRecorder stores per-task workflow artifacts (plan snapshots, trace
// events). Engine operates with a nil recorder — all recorder calls are
// guarded by nil checks so engine unit tests compile and pass unchanged.
type ArtifactRecorder interface {
	// RecordTrace appends a structured event to the task's trace.jsonl stream.
	RecordTrace(taskID string, ev any) error
	// PutPlanSnapshot stores a raw markdown plan as an artifact for the task.
	PutPlanSnapshot(taskID, role, stepID, sourcePath, content string) error
	// PutGeneric stores an arbitrary named blob (e.g. a tamper-detection
	// report) as a generic artifact for the task. Local debug/audit only —
	// callers must scrub before surfacing the content on any public destination.
	PutGeneric(taskID, name, stepID, content string) error
}

// EvidenceRecorder persists durable per-task CompletionEvidence (deterministic
// check outcomes, structured test verdicts, review findings) consulted by the
// require_evidence step before a task may land. Engine operates with a nil
// recorder — every call site is nil-guarded, and require_evidence itself
// no-ops without one, so engine unit tests compile and pass unchanged.
type EvidenceRecorder interface {
	// AppendCriterion records one proof for a named criterion, replacing any
	// existing entry for the same criterion. Best-effort: callers (the
	// deterministic gate steps) must never let a recording failure alter
	// their own pass/fail outcome or timing.
	AppendCriterion(taskID string, entry evidence.CriterionEvidence) error
	// Evidence returns the task's current CompletionEvidence. A task with no
	// recorded evidence returns a zero value and no error.
	Evidence(taskID string) (evidence.CompletionEvidence, error)
}

// EvidenceDecision summarizes one require_evidence step outcome, passed to
// the hook installed via SetEvidenceDecisionHook — mirrors AdmissionDecision.
type EvidenceDecision struct {
	// Outcome is "verified" or "blocked".
	Outcome string
	// Reason is the full block reason on a "blocked" outcome, or "" on a
	// "verified" outcome.
	Reason string
}

// CompletionInfo is passed to the OnComplete callback when a workflow finishes.
type CompletionInfo struct {
	TaskID     string
	WorkflowID string
	Variables  map[string]string
}

type pendingRecovery struct {
	onDecline func()
}

// Engine executes workflow definitions against tasks.
type Engine struct {
	store            Repository
	tasks            TaskProvider
	agents           AgentLauncher
	pr               PRSurface
	execution        ExecutionSurface
	recorder         ArtifactRecorder
	evidenceRecorder EvidenceRecorder
	// evidence configures the require_evidence step (zero-value Enabled=false
	// matches config.EvidenceConfig's own default — see SetEvidenceConfig).
	evidence     config.EvidenceConfig
	evidenceHook func(TaskInfo, EvidenceDecision)
	onComplete   func(CompletionInfo)
	dispatchGate func(TaskInfo) bool
	// dispatchDisabled is stored negated so the zero value keeps a
	// struct-literal Engine dispatching, matching its behavior before this
	// gate existed. atomic.Bool because SetAutoDispatch (config/lifecycle
	// goroutines) and the read sites (startWorkflowCore, DispatchEvent,
	// HandleStatusChange, ResumeStalled — all called from agent/workflow
	// goroutines) run concurrently with no shared lock between them.
	dispatchDisabled atomic.Bool
	ownerID          string
	effectLeaseTTL   time.Duration
	clockMu          sync.RWMutex
	clock            clock.Clock
	logger           *slog.Logger
	ctx              context.Context
	drainCtx         context.Context
	mu               sync.Mutex
	inflightLocks    fsutil.KeyedLocker         // taskID → advance serializer (parallel-aware)
	routeLocks       fsutil.KeyedLocker         // taskID → serialize run_agent route publication vs completion reads
	dispatching      map[string]struct{}        // taskID → workflow-engine dispatch/resume attempt in progress before StartAgent owns the shared manager claim
	starting         map[string]struct{}        // taskID → StartWorkflowWithVars in progress
	humanAction      map[string]struct{}        // taskID → HandleHumanAction in progress
	pendingRoutes    map[string]string          // taskID+"\x00"+agentID → stepID while StartAgent succeeded but route persistence has not
	completing       map[string]int             // taskID → in-flight HandleAgentComplete calls (agent finished, completion not yet routed)
	cascadeDepth     map[string]int             // taskID → synchronous cascade hop depth (recursion guard)
	pendingRecovery  map[string]pendingRecovery // taskID → branch-conflict recovery deferred until the outer marker releases
	resumeError      *logging.ErrorThrottle
	demotionThrottle *logging.ErrorThrottle
	resumeSkip       *logging.InfoThrottle
	// shadowDivergence throttles workflow.reducer.shadow_* log lines (see
	// shadow_reducer.go) — Reduce runs read-only alongside every AdvanceStep
	// write and this keeps a persistently-diverging step type from flooding
	// the log once per completion.
	shadowDivergence *logging.ErrorThrottle
	// These four are written by the config-reload goroutine via their Set*
	// methods while dispatch reads them, so they are atomic rather than plain
	// fields. A -race run under concurrent reloads caught reviewLoopDisabled
	// and reviewRoundsPerHour; the other two have the identical shape and were
	// simply not hit by that workload.
	maxTestAttempts atomic.Int64 // generous testing backstop; recurring fingerprints escalate before this cap (0 → defaultTestAttempts)
	// reviewLoopDisabled: see SetReviewUntilClean. Inverted so the zero value
	// keeps the review→fix→review cycle running, matching
	// config.ReviewUntilClean's default of true.
	reviewLoopDisabled atomic.Bool
	// reviewRoundsPerHour bounds simple-task-review's review→fix→review
	// cycle through the same durable reviewbudget.Budget the inbound
	// PR-review dispatcher uses (internal/sybra's app_orchestrator.go) —
	// one owner for "is this task reviewing too much", not a separate
	// per-workflow-execution round cap. 0 → config.DefaultReviewRoundsPerHour;
	// negative disables the cap.
	reviewRoundsPerHour atomic.Int64
	// openPROnUnrunnableGate: see SetOpenPROnUnrunnableGate. Defaults to true
	// (set in NewEngine), matching config.TestingOpenPROnUnrunnableGateEnabled's
	// nil-is-true default.
	openPROnUnrunnableGate    atomic.Bool
	maxCheckpoints            int           // checkpoint handoff cap per step (0 → defaultMaxCheckpoints)
	verifyTimeout             time.Duration // verify_checks budget (0 → verifyChecksDefaultTimeout)
	verifyChecksSlots         chan struct{} // process-local verify_checks concurrency cap; lazily initialized for zero-value Engines in tests
	verifyChecksMaxConcurrent int           // verify_checks slot count (<=0 → falls back to 1); see SetVerifyChecksMaxConcurrent
	// abMu guards abTesting alone: the routing ticker hot-swaps the A/B config
	// via SetABTestingConfig while dispatch reads it (selectABVariant,
	// providerEligibilitySnapshot, demotion/shutout reporting). Kept separate
	// from mu so a config swap never contends with dispatch bookkeeping.
	abMu             sync.RWMutex
	abTesting        abtest.Config
	evalGate         *prompteval.Gate      // nil = offline eval verdicts do not gate A/B enrollment
	cohortObserved   abtest.CohortObserved // nil = every Canary-gated experiment fails closed to its baseline variant
	conflictRecovery func(taskID string) bool
	// dispatchComparator, when set, orders TaskInfo pairs for ResumeStalled's
	// per-tick dispatch scan, replacing the built-in
	// dispatchorder.Rank(status)-only sort. Wired by app_init.go so
	// agentqueue.Less can back the ordering without internal/workflow
	// importing internal/agentqueue (which imports internal/task — see
	// TaskInfo.Priority's doc comment). fn must return <0/0/>0 like
	// cmp.Compare. nil preserves the original status-rank-only ordering.
	dispatchComparator func() func(a, b TaskInfo) int
	// queueReconciler, when set, runs once per ResumeStalled tick before the
	// dispatch scan, pruning admission-queue items whose task has gone
	// missing, terminal, or already in-progress (agentqueue.Queue.Reconcile).
	// nil disables reconciliation (no queue wired).
	queueReconciler                  func()
	autoApprovePlansWithoutDecisions bool
	planAutoApproveHook              func(TaskInfo, string)
	// admission configures the admission_preflight step's oversize checks
	// (zero-value MaxAcceptanceCriteria/MaxChangeSurfaceFiles disables them,
	// matching config.AdmissionConfig's own default). SetAdmissionConfig's
	// doc comment covers Enabled.
	admission config.AdmissionConfig
	// admissionDecisionHook observes every admission_preflight outcome. It is
	// used by the app layer to write the admission.decided audit event
	// without making workflow import the audit package — mirrors
	// planAutoApproveHook.
	admissionDecisionHook func(TaskInfo, AdmissionDecision)
}

var effectOwnerSeq atomic.Uint64

// AdmissionDecision summarizes one admission_preflight step outcome, passed
// to the hook installed via SetAdmissionDecisionHook.
type AdmissionDecision struct {
	// Outcome is "admitted" or "blocked".
	Outcome string
	// RiskTier/PermissionTier echo the task's plan contract fields (empty
	// when no contract is present), so evaluation can correlate predicted
	// risk/clarity against the actual admission and eventual task outcome.
	RiskTier       string
	PermissionTier string
	// BlockerKind is the blocker.Kind string set on a "blocked" outcome
	// (empty on "admitted").
	BlockerKind string
	// Reason is the full block reason on a "blocked" outcome, or one of
	// "admitted" (checks ran and passed) / "disabled" (admission.Enabled is
	// false, no checks ran) on an "admitted" outcome — never empty, so
	// consumers can distinguish a real pass from a skipped check.
	Reason string
	// FailureCode is a stable categorical code for every outcome; Reason
	// remains display-only.
	FailureCode string
}

// defaultTestAttempts is the generous absolute backstop for the testing →
// in-progress re-implementation loop when SetTestingMaxAttempts was never
// called. Recurring grounded failure fingerprints are the primary
// non-convergence signal; this mirrors config.DefaultTestingMaxAttempts.
const defaultTestAttempts = config.DefaultTestingMaxAttempts

// defaultMaxCheckpoints caps checkpoint handoffs per workflow step when
// SetMaxCheckpoints was never called. Mirrors config.DefaultMaxCheckpoints.
const defaultMaxCheckpoints = config.DefaultMaxCheckpoints

// Dependencies contains the collaborators required by a production Engine.
// Keeping them in cohesive groups makes construction atomic: adding a new
// required collaborator changes this value and its validation instead of
// adding another order-dependent Set* call in the application wiring.
type Dependencies struct {
	PR        PRSurface
	Execution ExecutionSurface
}

// NewEngine creates a production workflow engine. Every collaborator whose
// absence would skip or disable workflow behavior is validated before the
// engine is returned.
func NewEngine(store Repository, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger, deps Dependencies) (*Engine, error) {
	missing := missingDependencyNames(
		namedDependency{"Store", store},
		namedDependency{"Tasks", tasks},
		namedDependency{"Agents", agents},
		namedDependency{"Logger", logger},
	)
	missing = append(missing, deps.missing()...)
	if len(missing) > 0 {
		return nil, fmt.Errorf("workflow: engine dependencies missing %s", strings.Join(missing, ", "))
	}
	return newEngine(store, tasks, agents, logger, deps), nil
}

// NewTestEngine creates a deliberately partial engine for focused tests. Tests
// can bind only the collaborators they exercise through the documented test
// seams; production must use NewEngine so missing dependencies fail at startup.
func NewTestEngine(store Repository, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return newEngine(store, tasks, agents, logger, Dependencies{
		Execution: ExecutionSurface{
			BranchSyncer: skippedBranchSyncer{},
			Checks:       emptyCheckConfigGetter{},
			ManualTests:  emptyManualTestConfigGetter{},
			CostBudget:   unlimitedCostBudgetChecker{},
		},
	})
}

func newEngine(store Repository, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger, deps Dependencies) *Engine {
	e := &Engine{
		store:            store,
		tasks:            tasks,
		agents:           agents,
		pr:               deps.PR,
		execution:        deps.Execution,
		ownerID:          newEffectOwnerID(),
		effectLeaseTTL:   defaultEffectLeaseTTL,
		clock:            clock.System{},
		logger:           logger,
		ctx:              context.Background(),
		dispatching:      make(map[string]struct{}),
		starting:         make(map[string]struct{}),
		humanAction:      make(map[string]struct{}),
		pendingRoutes:    make(map[string]string),
		completing:       make(map[string]int),
		cascadeDepth:     make(map[string]int),
		pendingRecovery:  make(map[string]pendingRecovery),
		resumeError:      logging.NewErrorThrottle(),
		demotionThrottle: logging.NewErrorThrottle(),
		resumeSkip:       logging.NewInfoThrottle(),
		shadowDivergence: logging.NewErrorThrottle(),
	}
	// atomic.Bool's zero value is false, so the documented default has to be
	// stored explicitly rather than set in the literal.
	e.openPROnUnrunnableGate.Store(true)
	return e
}

func newEffectOwnerID() string {
	return fmt.Sprintf("workflow-engine-%d-%d", time.Now().UTC().UnixNano(), effectOwnerSeq.Add(1))
}

// SetClock binds the clock used by workflow control-flow decisions. It is safe
// to replace while the engine is running. Record timestamps that do not
// influence control flow continue to use time.Now.
func (e *Engine) SetClock(c clock.Clock) {
	e.clockMu.Lock()
	e.clock = clock.Or(c)
	e.clockMu.Unlock()
}

func (e *Engine) now() time.Time {
	e.clockMu.RLock()
	c := e.clock
	e.clockMu.RUnlock()
	return clock.Or(c).Now().UTC()
}

// SetContext binds a parent context to the engine. Shell steps use
// context.WithTimeout(parent, shellTimeout) so they are cancelled when
// the parent context is cancelled (e.g. on app shutdown).
func (e *Engine) SetContext(ctx context.Context) { e.ctx = ctx }

// SetDrainContext binds the context that is cancelled when the app begins
// draining, ahead of the hard stop that cancels SetContext's context.
//
// Retry backoffs wait on this rather than e.ctx. A backoff is idle waiting,
// not accepted work: parking it on e.ctx made it outlive the whole drain,
// because the drain waits for goroutines to finish before the hard stop that
// cancels e.ctx ever fires — so the wait blocked on the cancellation it was
// itself delaying. Running steps keep e.ctx and still drain normally.
func (e *Engine) SetDrainContext(ctx context.Context) { e.drainCtx = ctx }

// drainContext returns the drain-tier context, falling back to e.ctx when no
// drain context is bound (tests, embedders that never begin a drain).
func (e *Engine) drainContext() context.Context {
	if e.drainCtx != nil {
		return e.drainCtx
	}
	return e.ctx
}

// SetDispatchGate installs a predicate that reports whether a task should run
// its workflow on this node. ResumeStalled skips any task the gate rejects — in
// leader-follower mode a task homed on a remote follower executes there, and the
// leader only mirrors its state. A nil gate (the default) runs every task.
func (e *Engine) SetDispatchGate(gate func(TaskInfo) bool) { e.dispatchGate = gate }

// SetAutoDispatch turns workflow dispatch on or off for this instance. It is
// the single chokepoint behind Sybra's agent-only instance role. Every workflow
// start funnels through startWorkflowCore, which is where the flag is checked,
// so StartWorkflow*, DispatchEvent, ReplaceWorkflow and ReplaceWorkflowForEvent
// are all covered; HandleStatusChange and ResumeStalled check it too, to avoid
// the scan. A caller cannot start a workflow by reaching past it. Gating the
// call sites instead was tried and leaked three times — the engine has callers
// in TaskService, review, completion, PR integrations, promptlab, planning and
// the watcher.
//
// Deliberately blunt: it stops operator-initiated workflow starts too, not just
// automatic ones. An agent-only instance runs agents, not workflows; direct
// agent starts (App.StartAgent, sybra-cli) never touch the engine and keep
// working. Set orchestrator.scheduler_enabled true to opt an agent-only
// instance back into workflows.
func (e *Engine) SetAutoDispatch(on bool) { e.dispatchDisabled.Store(!on) }

// AutoDispatchEnabled reports whether this instance dispatches workflows. The
// gate in startWorkflowCore is what actually enforces it; this lets a caller
// avoid announcing an auto-start that is about to be refused, and avoid
// spawning a goroutine that would only no-op.
func (e *Engine) AutoDispatchEnabled() bool { return !e.dispatchDisabled.Load() }

// SetAutoApprovePlansWithoutDecisions enables automatic approval of validated
// simple-task plans whose decision sidecar explicitly says there are no open
// human decisions.
func (e *Engine) SetAutoApprovePlansWithoutDecisions(enabled bool) {
	e.autoApprovePlansWithoutDecisions = enabled
}

// SetPlanAutoApproveHook installs an observer for auto-approved plans. It is
// used by the app layer to write audit events without making workflow import
// the audit package.
func (e *Engine) SetPlanAutoApproveHook(hook func(TaskInfo, string)) {
	e.planAutoApproveHook = hook
}

// SetAdmissionConfig wires the admission_preflight step's oversize limits.
// Enabled defaults false in a zero-value config (matching every other
// Engine dependency's nil-safe default); the app layer resolves
// config.AdmissionConfig's own default-true before calling this, so an
// unwired Engine (e.g. in unit tests) safely runs the step as a no-op.
func (e *Engine) SetAdmissionConfig(cfg config.AdmissionConfig) {
	e.admission = cfg
}

// SetAdmissionDecisionHook installs an observer for every admission_preflight
// outcome. It is used by the app layer to write the admission.decided audit
// event without making workflow import the audit package — mirrors
// SetPlanAutoApproveHook.
func (e *Engine) SetAdmissionDecisionHook(hook func(TaskInfo, AdmissionDecision)) {
	e.admissionDecisionHook = hook
}

// Defs returns the workflow definition store.
func (e *Engine) Defs() Repository { return e.store }

// PRSurface is the pull-request dependency group: every collaborator the
// engine needs to open, find, link, close and re-review a PR. NewEngine
// validates the required members together before publishing an Engine;
// PushPreflighter is the sole optional member and retains its runtime fallback.
type PRSurface struct {
	Linker           PRLinker
	ReviewRequester  PRReviewRequester
	StateFetcher     PRStateFetcher
	HeadFetcher      PRHeadFetcher
	PushPreflighter  PushCredentialPreflighter
	Creator          PRCreator
	Closer           PRCloser
	Finder           PRFinder
	AnyStateFinder   PRAnyStateFinder
	ExistenceChecker PRExistenceChecker
	ContentGenerator PRContentGenerator
}

// ExecutionSurface groups the non-PR collaborators used to inspect and
// mutate a task checkout and to run deterministic workflow steps.
type ExecutionSurface struct {
	Worktrees            WorktreeGetter
	SidecarDir           SidecarDirResolver
	AttemptNotes         AttemptNoteAppender
	BranchSyncer         BranchSyncer
	Checks               CheckConfigGetter
	ManualTests          ManualTestConfigGetter
	Classifier           TaskClassifier
	CostBudget           CostBudgetChecker
	AttemptWorktrees     AttemptWorktreeManager
	Verification         VerificationWorkspaceManager
	VerificationCommands VerificationCommandRunner
	PostRun              reconcile.Runner
}

func (d Dependencies) missing() []string {
	missing := d.PR.missing()
	missing = append(missing, missingDependencyNames(
		namedDependency{"Execution.Worktrees", d.Execution.Worktrees},
		namedDependency{"Execution.SidecarDir", d.Execution.SidecarDir},
		namedDependency{"Execution.AttemptNotes", d.Execution.AttemptNotes},
		namedDependency{"Execution.BranchSyncer", d.Execution.BranchSyncer},
		namedDependency{"Execution.Checks", d.Execution.Checks},
		namedDependency{"Execution.ManualTests", d.Execution.ManualTests},
		namedDependency{"Execution.Classifier", d.Execution.Classifier},
		namedDependency{"Execution.CostBudget", d.Execution.CostBudget},
		namedDependency{"Execution.AttemptWorktrees", d.Execution.AttemptWorktrees},
		namedDependency{"Execution.Verification", d.Execution.Verification},
		namedDependency{"Execution.VerificationCommands", d.Execution.VerificationCommands},
		namedDependency{"Execution.PostRun", d.Execution.PostRun},
	)...)
	return missing
}

func (s PRSurface) missing() []string {
	return missingDependencyNames(
		namedDependency{"PR.Linker", s.Linker},
		namedDependency{"PR.ReviewRequester", s.ReviewRequester},
		namedDependency{"PR.StateFetcher", s.StateFetcher},
		namedDependency{"PR.HeadFetcher", s.HeadFetcher},
		namedDependency{"PR.Creator", s.Creator},
		namedDependency{"PR.Closer", s.Closer},
		namedDependency{"PR.Finder", s.Finder},
		namedDependency{"PR.AnyStateFinder", s.AnyStateFinder},
		namedDependency{"PR.ExistenceChecker", s.ExistenceChecker},
		namedDependency{"PR.ContentGenerator", s.ContentGenerator},
	)
}

type namedDependency struct {
	name  string
	value any
}

func missingDependencyNames(deps ...namedDependency) []string {
	var missing []string
	for _, dep := range deps {
		if isNilDependency(dep.value) {
			missing = append(missing, dep.name)
		}
	}
	return missing
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// setPRSurfaceForTest validates and replaces the PR group in focused tests.
func (e *Engine) setPRSurfaceForTest(s PRSurface) error {
	missing := s.missing()
	if len(missing) > 0 {
		return fmt.Errorf("workflow: PR surface missing %s", strings.Join(missing, ", "))
	}
	e.pr = s
	return nil
}

// SetPRLinker is a test seam for packages that exercise PR linking through the
// public Engine API. Production supplies the complete PRSurface to NewEngine.
func (e *Engine) SetPRLinker(l PRLinker) { e.pr.Linker = l }

// setPRReviewRequesterForTest wires an implementation used by rerequest_review.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRReviewRequesterForTest(r PRReviewRequester) { e.pr.ReviewRequester = r }

// setPRStateFetcherForTest wires an implementation used by `route_pr_fix_result` to
// re-probe the live PR before parking human-required. Leaving it unset skips
// the re-probe and trusts the agent's sentinel as-is.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRStateFetcherForTest(f PRStateFetcher) { e.pr.StateFetcher = f }

// setPRHeadFetcherForTest wires an implementation used by `push_branch` to verify a
// push landed. Leaving it unset skips the verification.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRHeadFetcherForTest(f PRHeadFetcher) { e.pr.HeadFetcher = f }

// setPushCredentialPreflighterForTest wires the push-auth preflight used before
// `push_branch` and `create_pr` attempt a real git push. Leaving it unset uses
// project.PreflightPushCredentials.
func (e *Engine) setPushCredentialPreflighterForTest(p PushCredentialPreflighter) {
	e.pr.PushPreflighter = p
}

// SetPRCreator is a test seam for deterministic PR-creation scenarios.
// Production supplies the complete PRSurface to NewEngine.
func (e *Engine) SetPRCreator(c PRCreator) { e.pr.Creator = c }

// setPRCloserForTest wires best-effort cleanup for superseded PRs after a task is
// relinked to a replacement PR.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRCloserForTest(c PRCloser) { e.pr.Closer = c }

// SetPRFinder is a test seam for create_pr idempotency scenarios. Production
// supplies the complete PRSurface to NewEngine.
func (e *Engine) SetPRFinder(f PRFinder) { e.pr.Finder = f }

// setPRAnyStateFinderForTest wires the all-state branch lookup used by create_pr's
// squash-merge duplicate guard. Leaving it unset skips the guard.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRAnyStateFinderForTest(f PRAnyStateFinder) { e.pr.AnyStateFinder = f }

// setPRExistenceCheckerForTest wires the pr_number-belongs-to-repo verification used
// by link_pr_and_review's Path 1. Leaving it unset skips the check and
// trusts task.pr_number outright, matching the engine's pre-existing
// behavior.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRExistenceCheckerForTest(c PRExistenceChecker) { e.pr.ExistenceChecker = c }

// setPRContentGeneratorForTest wires the LLM-backed title/body drafter used by the
// `create_pr` step. Leaving it unset falls back to a templated title/body.
// Test seam: production wires this through setPRSurfaceForTest.
func (e *Engine) setPRContentGeneratorForTest(g PRContentGenerator) { e.pr.ContentGenerator = g }

// SetWorktreeGetter is a test seam for steps that inspect a task checkout.
// Production supplies Worktrees through ExecutionSurface at construction.
func (e *Engine) SetWorktreeGetter(g WorktreeGetter) { e.execution.Worktrees = g }

// setSidecarDirResolverForTest late-binds the writable scratch directory used by
// verifier roles whose worktree is read-only.
func (e *Engine) setSidecarDirResolverForTest(r SidecarDirResolver) { e.execution.SidecarDir = r }

// resolveSidecarDir returns the writable scratch dir for taskID, or "" when no
// resolver is wired or it fails. Callers fall back to the worktree, which is
// the pre-#2791 behaviour, so a missing resolver degrades rather than breaks.
func (e *Engine) resolveSidecarDir(taskID string) string {
	if e == nil || e.execution.SidecarDir == nil {
		return ""
	}
	dir, err := e.execution.SidecarDir(taskID)
	if err != nil {
		e.logger.Warn("workflow.sidecar-dir.resolve", "task_id", taskID, "err", err)
		return ""
	}
	return strings.TrimSpace(dir)
}

// setAttemptNoteAppenderForTest wires the local NOTES.md writer used when testing
// routes a task back to implementation. Leaving it unset disables note seeding.
func (e *Engine) setAttemptNoteAppenderForTest(a AttemptNoteAppender) { e.execution.AttemptNotes = a }

// setBranchSyncerForTest wires a BranchSyncer used by the `sync_branch` step.
// Leaving it unset makes the step a no-op (skipped outcome).
func (e *Engine) setBranchSyncerForTest(s BranchSyncer) { e.execution.BranchSyncer = s }

// setCheckConfigGetterForTest wires the project codegen/verify resolver used by the
// `codegen_gate` and `verify_checks` steps. Leaving it unset makes those steps
// no-ops.
func (e *Engine) setCheckConfigGetterForTest(g CheckConfigGetter) { e.execution.Checks = g }

// setManualTestConfigGetterForTest wires the manual-test surface resolver used by
// testing prompts. Leaving it unset makes the prompt rely on repo discovery.
func (e *Engine) setManualTestConfigGetterForTest(g ManualTestConfigGetter) {
	e.execution.ManualTests = g
}

// SetVerifyTimeout overrides the verify_checks time budget. Zero keeps the
// default (verifyChecksDefaultTimeout). Used by tests for a short budget.
func (e *Engine) SetVerifyTimeout(d time.Duration) { e.verifyTimeout = d }

// SetVerifyChecksMaxConcurrent overrides the process-local verify_checks slot
// count. <=0 falls back to a single slot (the pre-existing behavior), so a
// zero-value Engine and callers that never invoke this setter are unchanged.
// Only takes effect for the slot channel created by the next verify_checks
// dispatch after this call — verifyChecksSlot lazily allocates the channel
// once and never resizes it, so this must be set before the engine's first
// verify_checks dispatch to have any effect. The app layer's config registry
// marks agent.verify_checks_max_concurrent restart-only for this reason
// (mirrors agent.evidence — see internal/sybra/config_registry.go).
func (e *Engine) SetVerifyChecksMaxConcurrent(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.verifyChecksMaxConcurrent = n
}

// SetTaskClassifier is a test seam for deterministic classification paths.
// Production supplies Classifier through ExecutionSurface at construction.
func (e *Engine) SetTaskClassifier(c TaskClassifier) { e.execution.Classifier = c }

// SetArtifactRecorder wires an ArtifactRecorder that captures per-task
// workflow artifacts (plan snapshots, trace events). Leaving it unset
// disables artifact recording — all calls are nil-guarded so engine unit
// tests remain unchanged.
func (e *Engine) SetArtifactRecorder(r ArtifactRecorder) { e.recorder = r }

// SetEvidenceRecorder wires an EvidenceRecorder that captures per-task
// completion evidence for the require_evidence step. Leaving it unset
// disables evidence recording and makes require_evidence a no-op — all calls
// are nil-guarded so engine unit tests remain unchanged.
func (e *Engine) SetEvidenceRecorder(r EvidenceRecorder) { e.evidenceRecorder = r }

// SetEvidenceConfig wires the require_evidence step's Enabled flag. Leaving it
// unset (zero value) keeps the gate disabled, matching
// config.EvidenceConfig's own default-false.
func (e *Engine) SetEvidenceConfig(cfg config.EvidenceConfig) { e.evidence = cfg }

// SetEvidenceDecisionHook installs an observer for every require_evidence
// outcome. Used by the app layer to write the completion_evidence.verified/
// blocked audit events without making internal/workflow import internal/audit
// — mirrors SetAdmissionDecisionHook.
func (e *Engine) SetEvidenceDecisionHook(hook func(TaskInfo, EvidenceDecision)) {
	e.evidenceHook = hook
}

// SetCostBudgetChecker is a test seam for best-of-N budget paths. Production
// supplies CostBudget through ExecutionSurface at construction.
func (e *Engine) SetCostBudgetChecker(c CostBudgetChecker) { e.execution.CostBudget = c }

// SetAttemptWorktreeManager is a test seam for best-of-N worktree paths.
// Production supplies AttemptWorktrees through ExecutionSurface at
// construction.
func (e *Engine) SetAttemptWorktreeManager(m AttemptWorktreeManager) {
	e.execution.AttemptWorktrees = m
}

// SetOnComplete registers a callback fired when a workflow reaches the
// completed state. Used to clear external debounce trackers.
func (e *Engine) SetOnComplete(fn func(CompletionInfo)) { e.onComplete = fn }

// SetTestingMaxAttempts sets the generous absolute backstop for how many times
// a task may fail manual testing and bounce back to in-progress before
// route_test_result parks it human-required. Recurring grounded failure
// fingerprints still escalate independently of this count. Values <= 0 fall
// back to defaultTestAttempts.
func (e *Engine) SetTestingMaxAttempts(n int) { e.maxTestAttempts.Store(int64(n)) }

// SetReviewUntilClean controls whether simple-task-review re-reviews after
// every fix until the verdict is CLEAN (true, the default) or runs a single
// review pass per task (false).
func (e *Engine) SetReviewUntilClean(v bool) { e.reviewLoopDisabled.Store(!v) }

// SetReviewRoundsPerHour sets the rolling-hour review-role dispatch cap
// simple-task-review's detect_tampering step checks before looping back for
// another review→fix round — the same resolved limit
// (config.AgentDefaults.ReviewRoundsPerHourLimit) the inbound PR-review
// dispatcher enforces. 0 falls back to config.DefaultReviewRoundsPerHour;
// negative disables the cap.
func (e *Engine) SetReviewRoundsPerHour(n int) { e.reviewRoundsPerHour.Store(int64(n)) }

// SetOpenPROnUnrunnableGate controls whether execRouteTestResult opens a PR
// (ready-pr) instead of escalating to human-required once a testing cycle
// exhausts its auto-retry budget on an infra_failure outcome — i.e. the
// manual gate itself could not be run (harness/tooling limitation), not a
// product defect. Defaults to true (see NewEngine); wired from
// config.TestingOpenPROnUnrunnableGateEnabled in app_init.go.
func (e *Engine) SetOpenPROnUnrunnableGate(v bool) { e.openPROnUnrunnableGate.Store(v) }

// ReviewUntilClean, ReviewRoundsPerHour, TestingMaxAttempts and
// OpenPROnUnrunnableGate read back what the corresponding Set* stored. They
// exist because these values live in atomics: the app layer's reload tests
// previously reflected into the plain fields, which cannot work on an
// atomic.Bool, and reflecting into one is worse than a getter.
func (e *Engine) ReviewUntilClean() bool { return !e.reviewLoopDisabled.Load() }

func (e *Engine) ReviewRoundsPerHour() int { return int(e.reviewRoundsPerHour.Load()) }

func (e *Engine) TestingMaxAttempts() int { return int(e.maxTestAttempts.Load()) }

func (e *Engine) OpenPROnUnrunnableGate() bool { return e.openPROnUnrunnableGate.Load() }

// SetMaxCheckpoints sets how many checkpoint handoffs a workflow step may
// spend before the task is parked human-required. Values <= 0 fall back to
// defaultMaxCheckpoints.
func (e *Engine) SetMaxCheckpoints(n int) { e.maxCheckpoints = n }

// SetABTestingConfig wires deterministic A/B assignment for run_agent steps.
// Safe to call concurrently with dispatch: the routing ticker hot-swaps this
// live via applyRoutingWeights.
func (e *Engine) SetABTestingConfig(cfg abtest.Config) {
	e.abMu.Lock()
	e.abTesting = cfg
	e.abMu.Unlock()
}

// abTestingConfig returns the current A/B config under the read lock. The
// returned value shares abTesting's backing experiment slice, but every writer
// (SetABTestingConfig / mergeWeights) publishes a freshly built slice rather
// than mutating in place, so an in-flight reader iterating this snapshot is
// unaffected by a concurrent swap.
func (e *Engine) abTestingConfig() abtest.Config {
	e.abMu.RLock()
	defer e.abMu.RUnlock()
	return e.abTesting
}

// SetEvalGate wires a prompteval.Gate so stored offline eval verdicts block
// online A/B enrollment for digested variants. Leaving it unset (nil)
// preserves prior behavior: no offline-eval gating.
func (e *Engine) SetEvalGate(gate *prompteval.Gate) { e.evalGate = gate }

// SetCohortObserved wires the canary-cohort readiness predicate selectABVariant
// consults for experiments with an abtest.CanaryPolicy — sourced from the
// evaluation/routing services' own resolved-run counts and freshness verdict.
// Leaving it unset (nil) fails every canary-gated experiment closed to its
// baseline variant.
func (e *Engine) SetCohortObserved(fn abtest.CohortObserved) { e.cohortObserved = fn }

// SetConflictRecovery wires the autonomous branch-conflict recovery callback
// (review.Handler.RecoverStaleBranchConflict) used by push_branch/create_pr
// when a push diverges from remote. Same callback agentorch wires for
// worktree-prep rebase failures — reused here so a self-inflicted divergence
// discovered at push time (e.g. a reused worktree rebased out from under an
// earlier merge-based push) gets the same autonomous fix instead of an
// unconditional human escalation. Leaving it unset preserves the prior
// behavior: any divergence flips straight to human-required.
func (e *Engine) SetConflictRecovery(fn func(taskID string) bool) { e.conflictRecovery = fn }

// SetDivergenceRecovery is a backward-compatible alias for SetConflictRecovery.
// Older tests and callers still use the pre-rename name for the same
// branch-divergence recovery hook.
func (e *Engine) SetDivergenceRecovery(fn func(taskID string) bool) {
	e.SetConflictRecovery(fn)
}

// SetDispatchComparator wires a total-order comparator for ResumeStalled's
// per-tick task scan, replacing the built-in dispatchorder.Rank(status)-only
// sort. fn must return <0/0/>0 like cmp.Compare. Leaving it unset (nil)
// preserves the original status-rank-only ordering.
func (e *Engine) SetDispatchComparator(factory func() func(a, b TaskInfo) int) {
	e.dispatchComparator = factory
}

// SetQueueReconciler wires a hook invoked once per ResumeStalled tick, before
// sorting/dispatch, to prune admission-queue items whose task has gone
// missing, terminal, or already in-progress. Leaving it unset (nil) disables
// reconciliation.
func (e *Engine) SetQueueReconciler(fn func()) {
	e.queueReconciler = fn
}

func (e *Engine) withManualTestConfig(t TaskInfo) TaskInfo {
	if t.ID == "" {
		return t
	}
	t.ManualTest = e.execution.ManualTests.ManualTestConfig(t.ID)
	return t
}
