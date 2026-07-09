package workflow

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/prompteval"
)

const (
	maxSyncSteps   = 100 // depth limit for synchronous step chains
	maxStepHistory = 50  // max step records kept per execution
	shellTimeout   = 30 * time.Second
)

// TaskInfo is the subset of task data the engine needs.
type TaskInfo struct {
	ID                    string
	Title                 string
	Status                string
	StatusReason          string
	Tags                  []string
	AgentMode             string
	ProjectID             string
	ProjectType           string
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
	StartedAt              time.Time
	ProtocolViolation      string
	TestOutcome            string
	TestFailureFingerprint string
	HeadSHA                string
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	ListTasks() ([]TaskInfo, error)
	UpdateTaskStatus(id, status, reason string) error
	UpdateTaskPR(id string, prNumber int) error
	MarkTaskReviewed(id string) error
	MarkAgentRunProtocolViolation(taskID, agentID, violation string) error
	MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error
	AppendTaskBody(id, content string) error
	// ReplaceTaskBody overwrites the task's full body. Used by the test-route
	// step to archive/strip a stale "## Test Failures" section before
	// appending a new one, so at most one such heading is ever live in the
	// body (see stripTestFailuresSections).
	ReplaceTaskBody(id, body string) error
	SetWorkflow(id string, wf *Execution) error
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
	//   "plan_draft.<name>" — raw per-provider plan during dual-/N-provider
	//       planning; <name> is typically the parallel child step ID. The
	//       engine derives <name> from the step ID when the YAML kind is
	//       a bare "plan_draft".
	WriteSidecar(id, kind, content string) error
}

// WorktreeGetter resolves the filesystem path of a task's git worktree.
// Returns (path, true) when a worktree exists for the task; ("", false) when
// none is found. Implementations may stat the path to confirm existence.
// Engine operates with a nil WorktreeGetter — verify_commits becomes a no-op.
type WorktreeGetter interface {
	GetWorktreePath(taskID string) (string, bool)
}

// BranchSyncer proactively reconciles a task's worktree branch with the
// project's default branch. Used by the `sync_branch` step. Engine operates
// with a nil BranchSyncer — the step then records a skipped outcome and
// never blocks workflow advancement.
type BranchSyncer interface {
	SyncTaskBranch(ctx context.Context, taskID string) (result string, err error)
}

// CheckConfigGetter resolves a task's verify-suite commands (the project's
// deterministic tests/typecheck), merged from repo `.sybra.yaml` and the
// app-level project config. Returns nil/empty when the task has no verify
// suite configured — the verify_checks step then becomes a no-op. Engine
// operates with a nil getter (step skips), so unit tests need not wire one.
type CheckConfigGetter interface {
	VerifyCommands(ctx context.Context, taskID string) []string
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

// CompletionInfo is passed to the OnComplete callback when a workflow finishes.
type CompletionInfo struct {
	TaskID     string
	WorkflowID string
	Variables  map[string]string
}

// agentEntry records which task and step an agent was spawned for.
type agentEntry struct {
	taskID string
	stepID string
}

// Engine executes workflow definitions against tasks.
type Engine struct {
	store            *Store
	tasks            TaskProvider
	agents           AgentLauncher
	prLinker         PRLinker
	prReviewers      PRReviewRequester
	prStates         PRStateFetcher
	worktrees        WorktreeGetter
	branchSyncer     BranchSyncer
	checks           CheckConfigGetter
	manualTests      ManualTestConfigGetter
	recorder         ArtifactRecorder
	onComplete       func(CompletionInfo)
	logger           *slog.Logger
	ctx              context.Context
	mu               sync.Mutex
	inflightMutexes  map[string]*sync.Mutex // taskID → advance serializer (parallel-aware)
	dispatching      map[string]struct{}    // taskID → dispatch in progress
	starting         map[string]struct{}    // taskID → StartWorkflowWithVars in progress
	humanAction      map[string]struct{}    // taskID → HandleHumanAction in progress
	agentSteps       map[string]agentEntry  // agentID → {taskID, stepID}
	dispatchingStep  map[string]int         // "taskID|stepID" → run_agent dispatches in flight; held until execRunAgent returns, agentID not yet assigned
	cascadeDepth     map[string]int         // taskID → synchronous cascade hop depth (recursion guard)
	resumeError      *logging.ErrorThrottle
	demotionThrottle *logging.ErrorThrottle
	maxTestAttempts  int           // testing → re-implement loop cap (0 → defaultTestAttempts)
	verifyTimeout    time.Duration // verify_checks budget (0 → verifyChecksDefaultTimeout)
	abTesting        abtest.Config
	evalGate         *prompteval.Gate // nil = offline eval verdicts do not gate A/B enrollment
}

// defaultTestAttempts caps the testing → in-progress re-implementation loop
// when SetTestingMaxAttempts was never called. Mirrors
// config.DefaultTestingMaxAttempts directly.
const defaultTestAttempts = config.DefaultTestingMaxAttempts

// NewEngine creates a workflow engine.
func NewEngine(store *Store, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return &Engine{
		store:            store,
		tasks:            tasks,
		agents:           agents,
		logger:           logger,
		ctx:              context.Background(),
		inflightMutexes:  make(map[string]*sync.Mutex),
		dispatching:      make(map[string]struct{}),
		starting:         make(map[string]struct{}),
		humanAction:      make(map[string]struct{}),
		agentSteps:       make(map[string]agentEntry),
		dispatchingStep:  make(map[string]int),
		cascadeDepth:     make(map[string]int),
		resumeError:      logging.NewErrorThrottle(),
		demotionThrottle: logging.NewErrorThrottle(),
	}
}

// SetContext binds a parent context to the engine. Shell steps use
// context.WithTimeout(parent, shellTimeout) so they are cancelled when
// the parent context is cancelled (e.g. on app shutdown).
func (e *Engine) SetContext(ctx context.Context) { e.ctx = ctx }

// Defs returns the workflow definition store.
func (e *Engine) Defs() *Store { return e.store }

// SetPRLinker wires an implementation of PRLinker used by the
// `ensure_pr_closes_issue` step. Leaving it unset makes the step a no-op.
func (e *Engine) SetPRLinker(l PRLinker) { e.prLinker = l }

// SetPRReviewRequester wires an implementation used by rerequest_review.
func (e *Engine) SetPRReviewRequester(r PRReviewRequester) { e.prReviewers = r }

// SetPRStateFetcher wires an implementation used by `route_pr_fix_result` to
// re-probe the live PR before parking human-required. Leaving it unset skips
// the re-probe and trusts the agent's sentinel as-is.
func (e *Engine) SetPRStateFetcher(f PRStateFetcher) { e.prStates = f }

// SetWorktreeGetter wires a WorktreeGetter used by the `verify_commits` step.
// Leaving it unset makes the step a no-op.
func (e *Engine) SetWorktreeGetter(g WorktreeGetter) { e.worktrees = g }

// SetBranchSyncer wires a BranchSyncer used by the `sync_branch` step.
// Leaving it unset makes the step a no-op (skipped outcome).
func (e *Engine) SetBranchSyncer(s BranchSyncer) { e.branchSyncer = s }

// SetCheckConfigGetter wires the project verify-suite resolver used by the
// `verify_checks` step. Leaving it unset makes the step a no-op.
func (e *Engine) SetCheckConfigGetter(g CheckConfigGetter) { e.checks = g }

// SetManualTestConfigGetter wires the manual-test surface resolver used by
// testing prompts. Leaving it unset makes the prompt rely on repo discovery.
func (e *Engine) SetManualTestConfigGetter(g ManualTestConfigGetter) { e.manualTests = g }

// SetVerifyTimeout overrides the verify_checks time budget. Zero keeps the
// default (verifyChecksDefaultTimeout). Used by tests for a short budget.
func (e *Engine) SetVerifyTimeout(d time.Duration) { e.verifyTimeout = d }

// SetArtifactRecorder wires an ArtifactRecorder that captures per-task
// workflow artifacts (plan snapshots, trace events). Leaving it unset
// disables artifact recording — all calls are nil-guarded so engine unit
// tests remain unchanged.
func (e *Engine) SetArtifactRecorder(r ArtifactRecorder) { e.recorder = r }

// SetOnComplete registers a callback fired when a workflow reaches the
// completed state. Used to clear external debounce trackers.
func (e *Engine) SetOnComplete(fn func(CompletionInfo)) { e.onComplete = fn }

// SetTestingMaxAttempts sets how many times a task may fail manual testing and
// bounce back to in-progress before route_test_result escalates it to
// human-required. Values <= 0 fall back to defaultTestAttempts.
func (e *Engine) SetTestingMaxAttempts(n int) { e.maxTestAttempts = n }

// SetABTestingConfig wires deterministic A/B assignment for run_agent steps.
func (e *Engine) SetABTestingConfig(cfg abtest.Config) { e.abTesting = cfg }

// SetEvalGate wires a prompteval.Gate so stored offline eval verdicts block
// online A/B enrollment for digested variants. Leaving it unset (nil)
// preserves prior behavior: no offline-eval gating.
func (e *Engine) SetEvalGate(gate *prompteval.Gate) { e.evalGate = gate }

func (e *Engine) withManualTestConfig(t TaskInfo) TaskInfo {
	if e.manualTests == nil || t.ID == "" {
		return t
	}
	t.ManualTest = e.manualTests.ManualTestConfig(t.ID)
	return t
}
