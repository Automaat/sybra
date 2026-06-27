package workflow

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/logging"
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
	Tags                  []string
	AgentMode             string
	ProjectID             string
	ProjectType           string
	HandoffSourceProvider string
	PRNumber              int
	Branch                string
	Body                  string
	Plan                  string
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
}

// AgentRunInfo is the engine-visible subset of a task's agent run.
type AgentRunInfo struct {
	AgentID           string
	Role              string
	Provider          string
	StartedAt         time.Time
	ProtocolViolation string
}

// TaskProvider reads and updates tasks.
type TaskProvider interface {
	GetTask(id string) (TaskInfo, error)
	ListTasks() ([]TaskInfo, error)
	UpdateTaskStatus(id, status, reason string) error
	UpdateTaskPR(id string, prNumber int) error
	MarkTaskReviewed(id string) error
	MarkAgentRunProtocolViolation(taskID, agentID, violation string) error
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

// CheckConfigGetter resolves a task's verify-suite commands (the project's
// deterministic tests/typecheck), merged from repo `.sybra.yaml` and the
// app-level project config. Returns nil/empty when the task has no verify
// suite configured — the verify_checks step then becomes a no-op. Engine
// operates with a nil getter (step skips), so unit tests need not wire one.
type CheckConfigGetter interface {
	VerifyCommands(taskID string) []string
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
	store           *Store
	tasks           TaskProvider
	agents          AgentLauncher
	prLinker        PRLinker
	prReviewers     PRReviewRequester
	worktrees       WorktreeGetter
	checks          CheckConfigGetter
	recorder        ArtifactRecorder
	onComplete      func(CompletionInfo)
	logger          *slog.Logger
	ctx             context.Context
	mu              sync.Mutex
	inflightMutexes map[string]*sync.Mutex // taskID → advance serializer (parallel-aware)
	dispatching     map[string]struct{}    // taskID → dispatch in progress
	starting        map[string]struct{}    // taskID → StartWorkflowWithVars in progress
	humanAction     map[string]struct{}    // taskID → HandleHumanAction in progress
	agentSteps      map[string]agentEntry  // agentID → {taskID, stepID}
	cascadeDepth    map[string]int         // taskID → synchronous cascade hop depth (recursion guard)
	resumeError     *logging.ErrorThrottle
	maxTestAttempts int           // testing → re-implement loop cap (0 → defaultTestAttempts)
	verifyTimeout   time.Duration // verify_checks budget (0 → verifyChecksDefaultTimeout)
	abTesting       abtest.Config
}

// defaultTestAttempts caps the testing → in-progress re-implementation loop
// when SetTestingMaxAttempts was never called. Mirrors
// config.DefaultTestingMaxAttempts (kept as a literal to avoid a config import).
const defaultTestAttempts = 3

// NewEngine creates a workflow engine.
func NewEngine(store *Store, tasks TaskProvider, agents AgentLauncher, logger *slog.Logger) *Engine {
	return &Engine{
		store:           store,
		tasks:           tasks,
		agents:          agents,
		logger:          logger,
		ctx:             context.Background(),
		inflightMutexes: make(map[string]*sync.Mutex),
		dispatching:     make(map[string]struct{}),
		starting:        make(map[string]struct{}),
		humanAction:     make(map[string]struct{}),
		agentSteps:      make(map[string]agentEntry),
		cascadeDepth:    make(map[string]int),
		resumeError:     logging.NewErrorThrottle(),
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

// SetWorktreeGetter wires a WorktreeGetter used by the `verify_commits` step.
// Leaving it unset makes the step a no-op.
func (e *Engine) SetWorktreeGetter(g WorktreeGetter) { e.worktrees = g }

// SetCheckConfigGetter wires the project verify-suite resolver used by the
// `verify_checks` step. Leaving it unset makes the step a no-op.
func (e *Engine) SetCheckConfigGetter(g CheckConfigGetter) { e.checks = g }

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
