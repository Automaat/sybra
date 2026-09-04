package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/toolledger"
)

type EmitFunc func(event string, data any)

// ErrSurvivalRegistry marks failures initializing restart-survival persistence.
var ErrSurvivalRegistry = errors.New("agent survival registry")

// defaultDeadAgentRetention is how long a completed agent's registry entry
// (output buffer, prompt, convo buffer) survives after markAgentDone runs,
// before Manager.deadAgentRetention evicts it. Bounds long-run memory growth
// (#1532) while leaving a window for callers to read final state
// (GetAgent/GetConvoOutput/Output) right after a terminal transition.
const defaultDeadAgentRetention = 10 * time.Minute

// ErrMaxConcurrentReached is returned by registerRunningAgent when the live
// agent count is already at MaxConcurrent. It is a transient, self-healing
// capacity condition (a slot frees when any running agent completes), so
// callers must park-and-retry rather than escalate — the workflow layer maps
// it to workflow.ErrAgentPoolBusy. Kept a sentinel (not a bare fmt.Errorf) so
// that mapping is errors.Is-based, not string-matched.
var ErrMaxConcurrentReached = errors.New("max concurrent agents reached")

// Guardrails defines per-agent execution limits.
type Guardrails struct {
	MaxCostUSD float64
	MaxTurns   int
	// MaxCheckpoints bounds how many checkpoint handoffs a single workflow step
	// may spend before the workflow parks human-required. Stored here so
	// guardrail reads reflect live config reloads in tests/UI snapshots.
	MaxCheckpoints int
	// TurnCostFraction is the fraction of MaxCostUSD below which a turns
	// escalation is auto-continued without human approval. Default 0.8.
	// Only effective when MaxCostUSD > 0.
	TurnCostFraction float64
	// TurnMultiplier scales the turn limit on each auto-continuation so
	// the agent gets progressively more turns. Default 2.
	TurnMultiplier float64
	// CheckpointOnTurnCeiling swaps the legacy raise-MaxTurns auto-continue for
	// a checkpoint-and-handoff on eligible code-author headless runs.
	CheckpointOnTurnCeiling bool
	// MaxSubagentEvents caps forked-subagent assistant events (parent_tool_use_id
	// non-empty) per run, independent of MaxTurns which only counts top-level
	// turns. 0 disables the ceiling. A breach hard-stops the run outright —
	// there is no auto-continue/human-escalation path, unlike MaxTurns.
	MaxSubagentEvents int
}

type Manager struct {
	agents        map[string]*Agent
	mu            sync.RWMutex
	liveCount     int
	ctx           context.Context
	emit          EmitFunc
	onComplete    func(ag *Agent)
	onReattach    func(ag *Agent) error
	logger        *slog.Logger
	logDir        string
	maxConcurrent int
	defaultProv   string
	approvalAddr  string // localhost:port for the HTTP tool approval server
	ghShimDir     string // dir holding the gh approval-guard shim, prepended to agent PATH
	// allowAmbientReviewAuth is an explicit operator opt-in for review agents
	// to use the host's gh credentials instead of a restricted App token.
	allowAmbientReviewAuth bool
	guardrails             Guardrails
	bashTimeoutMs          int
	retryWatchdog          int
	defaultModel           string
	fallbackModel          string
	// headlessSteerable gates whether headless claude runs launch with the
	// stdin/stream-json shape that accepts mid-run steer messages. See
	// RunConfig.HeadlessSteerable.
	headlessSteerable bool
	// roleEffort mirrors config agent.role_effort: an operator override of the
	// built-in per-role reasoning-effort baseline, keyed by role name. Read by
	// resolveReasoningEffort under mu.
	roleEffort map[string]string
	// runEnvironmentPreflight is the final application-owned admission gate
	// after provider, sandbox home, cache roots, and sandbox policy have been
	// resolved but before a provider process is registered or spawned.
	runEnvironmentPreflight RunEnvironmentPreflight
	// attemptAdmission owns durable task/worktree leases and hard provider
	// capacity. It is invoked only from RunContext's final launch chokepoint.
	attemptAdmission AttemptAdmission
	controlEvent     func(kind string, data map[string]any)

	defaultSandboxMode string
	// defaultSandboxReadMode is the read-visibility posture layered on top of
	// defaultSandboxMode; "off" unless an operator opts in (#2781).
	defaultSandboxReadMode string
	gate                   provider.HealthGate
	limitGate              LimitGate
	limitPolicy            limits.Policy
	limitSink              func(limits.Snapshot)
	evalPassed             abtest.EvalPassed
	cohortObserved         abtest.CohortObserved

	// liveByProvider tracks in-flight agent counts per provider, incremented
	// and decremented in lockstep with liveCount (registerRunningAgent,
	// markAgentDone, and the three ReattachAll restart paths) so
	// sum(liveByProvider) == liveCount always holds. Read by gateProvider to
	// steer dispatch away from an at-cap provider.
	liveByProvider map[string]int
	// reservedCount tracks capacity reservations held across expensive pre-run
	// work such as worktree setup. Reservations count against MaxConcurrent so
	// another dispatcher cannot steal the slot between "capacity looks free"
	// and registerRunningAgent's final claim.
	reservedCount int
	// liveByClass and reservedByClass mirror liveByProvider/reservedCount but
	// bucketed by WorkloadClass instead of provider, maintained at the same
	// three sites (registerRunningAgent, markAgentDone, ReattachAllContext) so
	// sum(liveByClass) == liveCount always holds. classFloors is the
	// configured per-class reserved minimum (agent.class_reservations,
	// default empty). admitClass reads all three to implement
	// reserve-with-borrowing admission.
	liveByClass     map[WorkloadClass]int
	reservedByClass map[WorkloadClass]int
	classFloors     map[WorkloadClass]int
	// maxInFlightPerProvider caps concurrent in-flight agents per provider.
	// Routing first tries a healthy peer; registerRunningAgent is the atomic
	// hard backstop that prevents concurrent selectors from overshooting it.
	// 0 disables the cap.
	maxInFlightPerProvider int
	// dispatchJitterMs bounds a uniform random delay applied before headless
	// dispatch to de-correlate a wave of same-tick starts. 0 disables jitter.
	dispatchJitterMs int
	// playwrightMCPEnabled mirrors config.PlaywrightMCPEnabled. Default-off:
	// see Manager.preparePlaywrightMCP for the full attach decision.
	playwrightMCPEnabled bool
	// playwrightMCPExtraArgs mirrors config.PlaywrightMCPExtraArgs, appended
	// verbatim to the Playwright MCP launch command.
	playwrightMCPExtraArgs []string
	executionBackend       ExecutionBackend
	localExecutionBackend  ExecutionBackend
	activeExecutions       map[string]activeExecution
	executionAgents        map[ExecutionHandle]string
	approvalResponder      func(string, bool) error
	// warnInertCapOnce guards the one-time inert-cap warning across both New
	// and every subsequent ReplaceRuntimeConfig call for this manager's
	// lifetime.
	warnInertCapOnce sync.Once

	// reg persists live-agent records so subprocesses can be reattached
	// after an app restart. nil disables survival (legacy behaviour).
	// Manager.mu guards only this pointer and survival config; registryStore
	// owns serialization of its on-disk Save/List/Delete operations.
	reg               survivalRegistry
	surviveRestart    bool
	surviveRestartDir string

	// sessionSink, when set, persists a crashed agent's captured session id
	// to its task's AgentRun on dead-reattach, so restart-stale recovery can
	// resume the conversation via --resume instead of cold-restarting. A
	// non-nil error means persistence failed and the registry record should
	// be retained for a later retry.
	sessionSink func(taskID, agentID, sessionID string) error

	// taskExists, when set, reports whether a task still exists. Used to
	// avoid recreating a zombie codex agent whose chat task was deleted.
	taskExists func(taskID string) bool

	taskStatus     func(taskID string) (string, bool)
	taskGeneration func(taskID string) (int64, bool)

	// toolLedger records every tool call every agent makes, whatever the
	// permission posture. Nil disables recording; Logger.Log tolerates it.
	toolLedger toolledger.Store
	// sandboxHome resolves the per-task sandbox SYBRA_HOME for a task-scoped
	// run. Required (non-nil) for any Run/StartAgent call with a non-empty
	// TaskID — see prepareRunConfig. nil is only valid when every caller is a
	// system/probe run with an empty TaskID (tests, health checks).
	sandboxHome func(taskID string) (string, error)
	// controlHome is exported to ordinary task-scoped agents. Verifiers never
	// receive it; they use the authenticated controlTarget/controlToken channel.
	controlHome   string
	controlTarget string
	controlToken  func(taskID, sandboxHome string) string

	// boardMu guards the board an agent's own CLI is pointed at, which the
	// process wiring sets after the manager exists.
	boardMu     sync.RWMutex
	boardTarget string
	boardToken  string
	boardCA     string

	ghAppToken         func() string
	ghVerifierAppToken func() string
	artifacts          *artifact.Store

	// deadAgentRetention bounds how long a completed agent stays in agents
	// after markAgentDone before being evicted. <= 0 evicts synchronously
	// (used by tests that need deterministic immediate eviction).
	deadAgentRetention time.Duration

	// dispatchClaims serializes agent dispatch per task. A claim is held for
	// the full duration of a StartAgent call — across the (multi-second)
	// worktree-preparation window during which the agent is not yet registered
	// in `agents`. Without it, two independent dispatchers (the workflow
	// cascade and the recovery loop) can both observe "no running agent" and
	// each start an agent on the same worktree. This is a pure in-flight lock:
	// it intentionally does NOT inspect running agents, so a step transition
	// dispatching its next agent inside the prior agent's onComplete (whose
	// `done` channel is not yet closed) is never blocked.
	dispatchClaims map[string]time.Time

	// queueNudge is a buffer-1 coalescing signal mirroring App.dispatchNudge:
	// at most one pending nudge is retained, so a burst of completions that
	// each free a slot collapses into a single pending wake-up for whatever
	// dispatch loop reads QueueNudge.
	queueNudge chan struct{}
}

// RunEnvironment is the exact provider-facing identity available immediately
// before a process starts. ScratchRoots are the mutable non-source roots that
// prepareRunConfig selected for this run.
type RunEnvironment struct {
	TaskID        string
	Role          Role
	Dir           string
	ReadOnlyPaths []string
	GitRoots      []string
	Provider      string
	SandboxMode   string
	ScratchRoots  []string
	// LocalCommand distinguishes deterministic shell checks from provider
	// agents. These checks need containment, but never provider capacity.
	LocalCommand bool
}

// RunEnvironmentPreflight certifies a prepared provider run before spawn.
type RunEnvironmentPreflight func(context.Context, RunEnvironment) error

// SetRunEnvironmentPreflight late-binds the application certification gate.
// Nil disables it for focused Manager tests and standalone consumers.
func (m *Manager) SetRunEnvironmentPreflight(preflight RunEnvironmentPreflight) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runEnvironmentPreflight = preflight
}

type LimitGate interface {
	ProviderAvailable(provider string, policy limits.Policy) (bool, string)
	ChooseProvider(requested string, candidates []string, healthy func(string) bool, policy limits.Policy) (string, string)
	ChooseSoftLimitedPeer(requested string, candidates []string, healthy func(string) bool, policy limits.Policy) (string, string)
}

// LimitGateOrNil wraps store as a LimitGate, returning a genuine nil
// interface when store is nil. Assigning a nil *limits.Store directly to a
// LimitGate field produces a non-nil interface holding a nil pointer, which
// defeats `== nil` guards and panics on the first method call.
func LimitGateOrNil(store *limits.Store) LimitGate {
	if store == nil {
		return nil
	}
	return store
}

// ManagerConfig contains startup-only wiring. Values that are intentionally
// live-editable are grouped in Runtime and updated via ReplaceRuntimeConfig.
type ManagerConfig struct {
	Runtime ManagerRuntimeConfig

	OnComplete        func(ag *Agent)
	OnReattach        func(ag *Agent) error
	ApprovalAddr      string
	SurviveRestartDir string
	SessionSink       func(taskID, agentID, sessionID string) error
	TaskExists        func(taskID string) bool
	TaskStatus        func(taskID string) (string, bool)
	TaskGeneration    func(taskID string) (int64, bool)
	LimitSink         func(limits.Snapshot)
	Artifacts         *artifact.Store
	AttemptAdmission  AttemptAdmission
	ControlEvent      func(kind string, data map[string]any)

	// SandboxHome resolves the per-task sandbox SYBRA_HOME directory for a
	// task-scoped run. Required for every fresh agent subprocess so it never
	// inherits an unset or operator SYBRA_HOME by default — see
	// Manager.prepareRunConfig. May be nil only when the manager is used
	// exclusively for system/probe runs with an empty TaskID.
	SandboxHome func(taskID string) (string, error)
	// ControlHome is exported to non-verifier task-scoped agents so sybra-cli
	// task commands reach the real operator store (typically config.HomeDir()).
	ControlHome string
	// ControlTarget and ControlToken route verifier CLI mutations through the
	// authenticated service boundary instead of exposing the operator store as
	// a writable filesystem path.
	ControlTarget string
	ControlToken  func(taskID, sandboxHome string) string
	// GhShimDir holds the `gh` approval-guard shim. It must sit outside the
	// agent's sandbox write roots (typically under config.HomeDir()) so a run
	// cannot overwrite its own guard. Empty disables the shim.
	GhShimDir string
	// AllowAmbientReviewAuth lets review agents use the host's gh credentials
	// when no restricted GitHub App verifier token is configured. This is an
	// explicit opt-in because those credentials may have broader authority.
	AllowAmbientReviewAuth bool
}

// ManagerRuntimeConfig holds settings that affect future runs and may change
// on config reload without rebuilding the manager.
type ManagerRuntimeConfig struct {
	MaxConcurrent   int
	DefaultProvider string
	DefaultModel    string
	BashTimeoutMs   int
	RetryWatchdog   int
	FallbackModel   string
	LimitGate       LimitGate
	LimitPolicy     limits.Policy
	SandboxMode     string
	SandboxReadMode string
	// MaxInFlightPerProvider caps concurrent in-flight agents per provider.
	// 0 disables the cap.
	MaxInFlightPerProvider int
	// DispatchJitterMs bounds a uniform random delay applied before headless
	// dispatch. 0 disables jitter.
	DispatchJitterMs int
	// HeadlessSteerable gates whether headless claude runs launch with the
	// stdin/stream-json shape that accepts mid-run steer messages. See
	// RunConfig.HeadlessSteerable.
	HeadlessSteerable bool
	// PlaywrightMCPEnabled mirrors config.PlaywrightMCPEnabled(). Default-off.
	PlaywrightMCPEnabled bool
	// PlaywrightMCPExtraArgs mirrors config.PlaywrightMCPExtraArgs().
	PlaywrightMCPExtraArgs []string
	// K8sJobsEnabled routes future headless runs to short-lived Kubernetes
	// Jobs. This is a PoC execution backend and is default-off.
	K8sJobsEnabled bool
	K8sJobs        K8sJobRunnerConfig
	// RoleEffort mirrors config agent.role_effort: an operator override of the
	// built-in per-role reasoning-effort baseline (Role.DefaultReasoningEffort),
	// keyed by role name. Invalid levels and unknown roles are ignored at
	// resolution time, falling back to the built-in baseline.
	RoleEffort map[string]string
	// ClassReservations mirrors config agent.class_reservations: a per-class
	// reserved minimum concurrent slot count. Keyed by WorkloadClass; unknown
	// keys and an over-budget sum are rejected at config validation, not here.
	// nil/empty reproduces pre-class-isolation behaviour exactly (admitClass
	// with all-zero floors is the plain liveCount+reserved<max rule).
	ClassReservations map[WorkloadClass]int
}

func NewManager(ctx context.Context, emit EmitFunc, logger *slog.Logger, logDir string, cfg ManagerConfig) (*Manager, error) {
	defaultProv, err := normalizeProviderName(cfg.Runtime.DefaultProvider)
	if err != nil {
		return nil, fmt.Errorf("default provider: %w", err)
	}
	m := &Manager{
		agents:                 make(map[string]*Agent),
		dispatchClaims:         make(map[string]time.Time),
		queueNudge:             make(chan struct{}, 1),
		liveByProvider:         make(map[string]int),
		liveByClass:            make(map[WorkloadClass]int),
		reservedByClass:        make(map[WorkloadClass]int),
		classFloors:            cloneClassFloors(cfg.Runtime.ClassReservations),
		ctx:                    ctx,
		emit:                   emit,
		onComplete:             cfg.OnComplete,
		onReattach:             cfg.OnReattach,
		logger:                 logger,
		logDir:                 logDir,
		approvalAddr:           cfg.ApprovalAddr,
		ghShimDir:              resolveGhShimDir(cfg.GhShimDir, logger),
		allowAmbientReviewAuth: cfg.AllowAmbientReviewAuth,
		defaultProv:            defaultProv,
		defaultModel:           cfg.Runtime.DefaultModel,
		maxConcurrent:          cfg.Runtime.MaxConcurrent,
		bashTimeoutMs:          cfg.Runtime.BashTimeoutMs,
		retryWatchdog:          cfg.Runtime.RetryWatchdog,
		fallbackModel:          cfg.Runtime.FallbackModel,
		limitGate:              cfg.Runtime.LimitGate,
		limitPolicy:            copyLimitPolicy(cfg.Runtime.LimitPolicy),
		limitSink:              cfg.LimitSink,
		artifacts:              cfg.Artifacts,
		attemptAdmission:       cfg.AttemptAdmission,
		controlEvent:           cfg.ControlEvent,
		sessionSink:            cfg.SessionSink,
		taskExists:             cfg.TaskExists,
		taskStatus:             cfg.TaskStatus,
		taskGeneration:         cfg.TaskGeneration,
		maxInFlightPerProvider: cfg.Runtime.MaxInFlightPerProvider,
		dispatchJitterMs:       cfg.Runtime.DispatchJitterMs,
		headlessSteerable:      cfg.Runtime.HeadlessSteerable,
		roleEffort:             maps.Clone(cfg.Runtime.RoleEffort),
		defaultSandboxMode:     cfg.Runtime.SandboxMode,
		defaultSandboxReadMode: cfg.Runtime.SandboxReadMode,
		sandboxHome:            cfg.SandboxHome,
		controlHome:            cfg.ControlHome,
		controlTarget:          cfg.ControlTarget,
		controlToken:           cfg.ControlToken,
		deadAgentRetention:     defaultDeadAgentRetention,
		playwrightMCPEnabled:   cfg.Runtime.PlaywrightMCPEnabled,
		playwrightMCPExtraArgs: cfg.Runtime.PlaywrightMCPExtraArgs,
		activeExecutions:       make(map[string]activeExecution),
		executionAgents:        make(map[ExecutionHandle]string),
	}
	m.localExecutionBackend = newCallbackExecutionBackend("local")
	m.executionBackend = m.localExecutionBackend
	if cfg.Runtime.K8sJobsEnabled {
		m.executionBackend = m.newK8sExecutionBackend(cfg.Runtime.K8sJobs)
	}
	m.warnInertCap(logger, m.maxInFlightPerProvider, m.limitGate)
	if cfg.SurviveRestartDir != "" {
		s, err := newRegistryStore(cfg.SurviveRestartDir)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSurvivalRegistry, err)
		}
		m.reg = s
		m.surviveRestart = true
		m.surviveRestartDir = cfg.SurviveRestartDir
	}
	return m, nil
}

// ClaimTaskDispatch reserves the right to dispatch an agent for taskID,
// returning false when a dispatch is already in flight for the same task. The
// caller MUST NOT start an agent on a false return, and MUST ReleaseTaskDispatch
// once dispatch finishes (success or failure) on a true return. This closes the
// window between dispatch start and agent registration where a concurrent
// dispatcher would otherwise see no running agent and start a duplicate.
// StaleDispatchClaimAge bounds how long a dispatch claim is honoured before
// it is treated as leaked and released. It is exported because callers that
// wait out a contended claim must keep their retry window strictly under it:
// the release is purely age-based and cannot tell a leaked claim from a live
// holder, so a waiter that retries past this age can be granted a claim that
// another dispatcher is still using.
const StaleDispatchClaimAge = 15 * time.Minute

func (m *Manager) ClaimTaskDispatch(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dispatchClaimHeldLocked(taskID, time.Now()) {
		return false
	}
	if m.dispatchClaims == nil {
		m.dispatchClaims = make(map[string]time.Time)
	}
	m.dispatchClaims[taskID] = time.Now()
	return true
}

// ReleaseStaleTaskDispatch releases an in-flight dispatch claim only when it
// has exceeded age. It returns true when a claim was removed.
func (m *Manager) ReleaseStaleTaskDispatch(taskID string, age time.Duration) bool {
	if age <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	claimedAt, held := m.dispatchClaims[taskID]
	if !held || time.Since(claimedAt) < age {
		return false
	}
	if m.logger != nil {
		m.logger.Warn("agent.dispatch-claim.stale-release", "task_id", taskID, "age", time.Since(claimedAt))
	}
	delete(m.dispatchClaims, taskID)
	return true
}

// ReleaseTaskDispatch releases a claim acquired by ClaimTaskDispatch. Safe to
// call for a task with no outstanding claim.
func (m *Manager) ReleaseTaskDispatch(taskID string) {
	m.mu.Lock()
	delete(m.dispatchClaims, taskID)
	m.mu.Unlock()
}

// IsDispatching reports whether a dispatch claim is currently held for
// taskID, by any caller of ClaimTaskDispatch/TryClaimDispatch — the workflow
// engine's execRunAgent, recovery.RestartStaleInProgress (via
// agentorch.Orchestrator), or a direct non-workflow StartAgent call. This is
// the single ground-truth answer to "is this task's next run already
// owned?" that every dispatcher can consult before deciding to redispatch,
// instead of each maintaining its own separate view of dispatch-in-flight
// state.
func (m *Manager) IsDispatching(taskID string) bool {
	return m.dispatchClaimHeld(taskID, time.Now())
}

// dispatchClaimHeld reports whether taskID has a non-stale dispatch claim.
// The common fresh/no-claim path only takes a read lock; stale claims upgrade
// to a write lock and re-check before deleting.
func (m *Manager) dispatchClaimHeld(taskID string, now time.Time) bool {
	m.mu.RLock()
	held, stale := m.dispatchClaimHeldReadLocked(taskID, now)
	m.mu.RUnlock()
	if !stale {
		return held
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatchClaimHeldLocked(taskID, now)
}

func (m *Manager) dispatchClaimHeldReadLocked(taskID string, now time.Time) (held, stale bool) {
	claimedAt, held := m.dispatchClaims[taskID]
	if !held {
		return false, false
	}
	if now.Sub(claimedAt) >= StaleDispatchClaimAge {
		return false, true
	}
	return true, false
}

// dispatchClaimHeldLocked reports whether taskID has a non-stale dispatch
// claim. Stale claims are deleted as a side effect so every caller that treats
// dispatchClaims as liveness uses the same self-healing semantics.
// Callers must hold m.mu for writing.
func (m *Manager) dispatchClaimHeldLocked(taskID string, now time.Time) bool {
	held, stale := m.dispatchClaimHeldReadLocked(taskID, now)
	if !stale {
		return held
	}
	claimedAt := m.dispatchClaims[taskID]
	age := now.Sub(claimedAt)
	if m.logger != nil {
		m.logger.Warn("agent.dispatch-claim.stale-release", "task_id", taskID, "age", age)
	}
	delete(m.dispatchClaims, taskID)
	return false
}

// DispatchClaim is a held per-task dispatch claim returned by
// TryClaimDispatch. It centralizes the release-at-most-once bookkeeping every
// dispatch call site used to hand-roll individually (a local `released bool`
// guarding a deferred delete): releasing early to unblock a nested
// same-task dispatch (e.g. branch-conflict recovery) can never race a
// deferred second release into clobbering a claim a different dispatcher has
// since acquired.
type DispatchClaim struct {
	manager  *Manager
	taskID   string
	released bool
}

// CapacityReservation holds one reserved agent-pool slot across pre-run work.
// Release is idempotent and nil-safe. A successful Run consumes the
// reservation instead of re-checking the cap, so the slot stays owned across
// worktree prep/setup.
type CapacityReservation struct {
	manager  *Manager
	counted  bool
	released bool
	// class is the WorkloadClass this reservation counted against in
	// reservedByClass. Always ClassImplementation today: TryHoldCapacity/
	// TryHoldCapacityWithLimit have exactly one caller (the workflow engine's
	// run_agent step), which is always an implementation dispatch. Widen this
	// to a caller-supplied class if a second caller ever reserves for a
	// different WorkloadClass.
	class WorkloadClass
}

// TryClaimDispatch reserves the right to dispatch an agent for taskID. On a
// true ok, the caller MUST release the returned claim exactly once dispatch
// finishes (success or failure) — typically via `defer claim.Release()` —
// and MAY call Release earlier to unblock a nested same-task dispatch before
// the deferred call runs, since Release is idempotent. On ok=false a dispatch
// is already in flight for the same task and the caller MUST NOT start an
// agent.
func (m *Manager) TryClaimDispatch(taskID string) (claim *DispatchClaim, ok bool) {
	if !m.ClaimTaskDispatch(taskID) {
		return nil, false
	}
	return &DispatchClaim{manager: m, taskID: taskID}, true
}

// Release releases the claim. Idempotent and nil-safe: a second call (or a
// call on a nil claim) is a no-op, so an early manual release followed by a
// deferred Release never double-deletes a claim a different dispatcher has
// since acquired.
func (c *DispatchClaim) Release() {
	if c == nil || c.released {
		return
	}
	c.released = true
	c.manager.ReleaseTaskDispatch(c.taskID)
}

func (r *CapacityReservation) consumeLocked() bool {
	if r == nil || r.released {
		return false
	}
	r.released = true
	if r.counted {
		if r.manager.reservedCount > 0 {
			r.manager.reservedCount--
		}
		if r.manager.reservedByClass[r.class] > 0 {
			r.manager.reservedByClass[r.class]--
		}
	}
	return true
}

// Release frees a reserved slot. Safe to call after a successful Run: the
// reservation is already consumed and this becomes a no-op.
func (r *CapacityReservation) Release() {
	if r == nil {
		return
	}
	m := r.manager
	if m == nil {
		return
	}
	shouldNudge := false
	m.mu.Lock()
	if !r.released {
		r.released = true
		if r.counted {
			if m.reservedCount > 0 {
				m.reservedCount--
				shouldNudge = true
			}
			if m.reservedByClass[r.class] > 0 {
				m.reservedByClass[r.class]--
			}
		}
	}
	m.mu.Unlock()
	if shouldNudge {
		m.signalQueueNudge()
	}
}

// ReplaceRuntimeConfig replaces the complete live runtime snapshot. Settings
// affect future Run calls and config reloads without mutating startup-only callbacks.
func (m *Manager) ReplaceRuntimeConfig(cfg ManagerRuntimeConfig) error {
	defaultProv, err := normalizeProviderName(cfg.DefaultProvider)
	if err != nil {
		return fmt.Errorf("default provider: %w", err)
	}
	m.mu.Lock()
	m.maxConcurrent = cfg.MaxConcurrent
	m.defaultProv = defaultProv
	m.defaultModel = cfg.DefaultModel
	m.bashTimeoutMs = cfg.BashTimeoutMs
	m.retryWatchdog = cfg.RetryWatchdog
	m.fallbackModel = cfg.FallbackModel
	m.limitGate = cfg.LimitGate
	m.limitPolicy = copyLimitPolicy(cfg.LimitPolicy)
	m.maxInFlightPerProvider = cfg.MaxInFlightPerProvider
	m.dispatchJitterMs = cfg.DispatchJitterMs
	m.headlessSteerable = cfg.HeadlessSteerable
	m.roleEffort = maps.Clone(cfg.RoleEffort)
	m.defaultSandboxMode = cfg.SandboxMode
	m.defaultSandboxReadMode = cfg.SandboxReadMode
	m.playwrightMCPEnabled = cfg.PlaywrightMCPEnabled
	m.playwrightMCPExtraArgs = cfg.PlaywrightMCPExtraArgs
	m.classFloors = cloneClassFloors(cfg.ClassReservations)
	if cfg.K8sJobsEnabled {
		m.executionBackend = m.newK8sExecutionBackend(cfg.K8sJobs)
	} else {
		m.executionBackend = m.localExecutionBackend
	}
	admission := m.attemptAdmission
	m.mu.Unlock()
	if updater, ok := admission.(AttemptLimitUpdater); ok {
		updater.ReplaceLimits(cfg.MaxConcurrent, cfg.MaxInFlightPerProvider)
	}
	m.warnInertCap(m.logger, cfg.MaxInFlightPerProvider, cfg.LimitGate)
	return nil
}

// warnInertCap logs once, across this manager's whole lifetime, when
// MaxInFlightPerProvider is configured but no LimitGate is wired up (e.g.
// limits.NewStore failed at startup), since gateProvider then skips the
// cap-redirect logic entirely and the cap has zero effect for as long as the
// gate stays nil.
func (m *Manager) warnInertCap(logger *slog.Logger, maxInFlightPerProvider int, limitGate LimitGate) {
	if maxInFlightPerProvider > 0 && limitGate == nil {
		m.warnInertCapOnce.Do(func() {
			logger.Warn("agent.max_in_flight_per_provider.inert", "max_in_flight_per_provider", maxInFlightPerProvider)
		})
	}
}

// InFlightByProvider returns a snapshot of in-flight agent counts by
// provider, kept in lockstep with RunningCount's liveCount.
func (m *Manager) InFlightByProvider() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return maps.Clone(m.liveByProvider)
}

// InFlightByClass returns a snapshot of in-flight agent counts by
// WorkloadClass, kept in lockstep with RunningCount's liveCount (mirrors
// InFlightByProvider). Keys are the string form of WorkloadClass so callers
// (metrics) do not need to import this package's type.
func (m *Manager) InFlightByClass() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int, len(m.liveByClass))
	for c, n := range m.liveByClass {
		out[string(c)] = n
	}
	return out
}

func (m *Manager) sessionSinkFn() func(taskID, agentID, sessionID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessionSink
}

func (m *Manager) taskExistsFn() func(taskID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.taskExists
}

func (m *Manager) taskStatusFn() func(taskID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.taskStatus
}

// survives reports whether restart survival is active.
func (m *Manager) survives() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.surviveRestart && m.reg != nil
}

// willDetach reports whether a run survives a restart. Headless runs survive
// as detached processes (the process runs independent of the parent once
// spawned). Single source of truth mirrored by the runner branches.
func willDetach(cfg RunConfig) bool {
	switch cfg.Mode {
	case "headless":
		return true
	default:
		return false
	}
}

// registry returns the survival registry (nil when survival is disabled).
func (m *Manager) registry() survivalRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reg
}

func (m *Manager) registryDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.surviveRestartDir
}

// saveRegistry snapshots the agent to disk and refreshes its durable attempt
// binding. Admission updates are intentionally independent of restart
// survival: a deployment may disable process reattachment while still using
// durable leases for dispatch ownership and capacity.
func (m *Manager) saveRegistry(ctx context.Context, a *Agent) {
	if err := m.persistRegistry(ctx, a); err != nil && a != nil {
		m.logger.Warn("agent.registry.save", "id", a.ID, "task_id", a.TaskID, "err", err)
	}
}

func (m *Manager) persistRegistry(ctx context.Context, a *Agent) error {
	reg := m.registry()
	if a == nil {
		return nil
	}
	rec := a.toRecord()
	rec.ProcStartedAt = processStartString(ctx, rec.PID)
	m.bindAndHeartbeatAttempt(ctx, a, rec.ProcStartedAt)
	if reg == nil {
		return nil
	}
	if err := reg.Save(rec); err != nil {
		return err
	}
	return nil
}

// signalKill terminates an agent's subprocess. Prefers the *exec.Cmd
// handle (live this lifetime); falls back to the PID for reattached
// agents that have no handle.
func (m *Manager) signalKill(a *Agent) {
	if cmd := a.GetCmd(); cmd != nil && cmd.Process != nil {
		stopWithSIGINT(cmd, a.done, stopSIGINTGrace)
		return
	}
	signalPID(a.GetPID(), stopSIGINTGrace)
}

// SetHealthGate wires in a provider health checker so Run() can refuse or
// failover when the requested provider is unhealthy. A nil gate disables the
// check entirely (tests, feature-disabled mode).
func (m *Manager) SetHealthGate(g provider.HealthGate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// SetEvalPassed wires the offline-eval enrollment predicate consulted by
// ApplyABVariant, so ad-hoc A/B dispatch sites gate digested variants on their
// stored eval verdict exactly like the workflow engine. A nil predicate (the
// default) leaves eval gating off.
func (m *Manager) SetEvalPassed(fn abtest.EvalPassed) {
	m.mu.Lock()
	m.evalPassed = fn
	m.mu.Unlock()
}

// SetCohortObserved wires the canary-cohort readiness predicate ApplyABVariant
// consults for experiments with a Canary policy — sourced from the
// evaluation/routing services' own resolved-run counts and freshness verdict
// (see evaluation.Trustworthy), so abtest and agent never need to know about
// evaluation directly. A nil predicate (the default) leaves every canary
// experiment fail-closed to its baseline variant.
func (m *Manager) SetCohortObserved(fn abtest.CohortObserved) {
	m.mu.Lock()
	m.cohortObserved = fn
	m.mu.Unlock()
}

func (m *Manager) SetGHAppToken(fn func() string) {
	m.mu.Lock()
	m.ghAppToken = fn
	m.mu.Unlock()
	if err := m.syncGHAppToken(); err != nil {
		m.logger.Error("agent.gh-shim.token", "err", err)
	}
}

func (m *Manager) SetGHVerifierAppToken(fn func() string) {
	m.mu.Lock()
	m.ghVerifierAppToken = fn
	m.mu.Unlock()
	if err := m.syncGHVerifierAppToken(); err != nil {
		m.logger.Error("agent.gh-shim.verifier-token", "err", err)
	}
}

func (m *Manager) LimitPolicy() limits.Policy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return copyLimitPolicy(m.limitPolicy)
}

func copyLimitPolicy(policy limits.Policy) limits.Policy {
	if policy.SubscriptionMonthlyUSD != nil {
		policy.SubscriptionMonthlyUSD = copyStringFloatMap(policy.SubscriptionMonthlyUSD)
	}
	if policy.ProviderEnabled != nil {
		policy.ProviderEnabled = copyStringBoolMap(policy.ProviderEnabled)
	}
	return policy
}

func copyStringFloatMap(in map[string]float64) map[string]float64 {
	return maps.Clone(in)
}

func copyStringBoolMap(in map[string]bool) map[string]bool {
	return maps.Clone(in)
}

// cloneClassFloors returns a non-nil defensive copy so admitClass can always
// index it directly without a nil-map guard, and so a caller's map cannot be
// mutated out from under the manager after ReplaceRuntimeConfig/NewManager.
func cloneClassFloors(in map[WorkloadClass]int) map[WorkloadClass]int {
	out := make(map[WorkloadClass]int, len(AllWorkloadClasses()))
	maps.Copy(out, in)
	return out
}

// ProviderRateLimited reports whether the named provider is currently in a
// rate-limit cooldown — distinct from logged-out / auth failure. A nil health
// gate (checks disabled, tests) reports false. An empty name resolves to the
// default provider. Recovery loops consult this to wait out a transient rate
// limit; auth failures deliberately do NOT count here so they keep taking the
// human-required path (a human must log in) instead of waiting indefinitely.
func (m *Manager) ProviderRateLimited(name string) bool {
	m.mu.RLock()
	g := m.gate
	if name == "" {
		name = m.defaultProv
	}
	m.mu.RUnlock()
	if g == nil {
		return false
	}
	return g.RateLimited(name)
}

// ProviderHealthy reports whether the named provider is currently usable —
// false for a probe-detected outage (health gate), a config-disabled
// provider (providers.<name>.enabled=false, via limitPolicy.ProviderEnabled),
// or a hard quota exhaustion (limitGate, e.g. a real "resets Jul 28" usage
// cap, not a soft threshold). The limitGate check matters here specifically
// because it's a different signal than the probe-based health gate: g's
// rate-limit cooldown defaults to 15 minutes (nowhere near a real multi-day
// quota reset), so without also consulting limitGate, A/B selection kept
// treating a hard-exhausted provider as eligible, re-picking it every retry
// only for resolveProviderDecision's own limitGate check to reject it at
// dispatch — burning a full worktree rebuild per cycle before the
// in-dispatch fallback can correct the stale selection. The config-disabled and
// quota checks are independent of the health gate so they still hold when
// providers.health_check.enabled=false and g is nil (checks disabled,
// tests) — otherwise ProviderHealthy would report true for a provider the
// admin explicitly disabled or that is verifiably out of quota. Empty name
// resolves to the default provider. A/B variant selection consults this so a
// disabled, quota-exhausted, or unhealthy provider is never picked as an
// eligible weighted variant.
func (m *Manager) ProviderHealthy(name string) bool {
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	if name == "" {
		name = m.defaultProv
	}
	m.mu.RUnlock()
	if enabled, ok := lp.ProviderEnabled[name]; ok && !enabled {
		return false
	}
	if lg != nil {
		if ok, reason := lg.ProviderAvailable(name, lp); !ok && !limits.IsSoftThresholdReason(reason) {
			// ProviderAvailable reports unavailable for soft session/weekly
			// thresholds too, not just a hard block — deliberately, so
			// resolveProviderDecision's softLimitLastResort can redirect to a
			// healthier peer without stranding a task on a provider that
			// still has real budget when no peer exists. ProviderHealthy
			// must only exclude on the hard reasons (rate-limit-reached,
			// provider-disabled), or a soft-thresholded provider would be
			// wrongly removed from A/B eligibility entirely instead of just
			// de-prioritized at dispatch time.
			return false
		}
	}
	if g == nil {
		return true
	}
	return g.IsHealthy(name)
}

// ProviderCanFailover reports whether the health/limit gates can route work
// away from the named provider right now.
func (m *Manager) ProviderCanFailover(name string) bool {
	m.mu.RLock()
	g := m.gate
	lg := m.limitGate
	lp := m.limitPolicy
	if name == "" {
		name = m.defaultProv
	}
	m.mu.RUnlock()
	prov, err := lookupProvider(name)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("agent.failover.provider", "provider", name, "err", err)
		}
		return false
	}
	resolved := prov.Name()
	enabled := func(p string) bool {
		return providerPolicyEnabled(lp, p)
	}
	healthy := func(p string) bool {
		return enabled(p) && (g == nil || g.IsHealthy(p))
	}
	if g != nil {
		alt := g.Failover(resolved)
		if alt != "" && healthy(alt) {
			return true
		}
	}
	if !enabled(resolved) && firstHealthyProvider(resolved, providerid.All(), healthy) != "" {
		return true
	}
	if lg == nil {
		return false
	}
	candidates := providerid.All()
	if alt, _ := lg.ChooseProvider(resolved, candidates, healthy, lp); alt != "" {
		return true
	}
	available, reason := lg.ProviderAvailable(resolved, lp)
	if available || limits.IsSoftThresholdReason(reason) {
		return false
	}
	// Mirror resolveProviderDecision's last-resort path: when no fully
	// available peer exists, a soft-threshold-limited peer is still a usable
	// failover target for a hard-blocked provider (e.g. rate limit reached).
	alt, _ := lg.ChooseSoftLimitedPeer(resolved, candidates, healthy, lp)
	return alt != ""
}

// ReportProviderSignal forwards a runner-side passive signal (rate-limit or
// auth failure) to the health gate. Safe to call with a nil gate.
func (m *Manager) ReportProviderSignal(name string, c provider.Classification) {
	m.mu.RLock()
	g := m.gate
	m.mu.RUnlock()
	if g == nil {
		return
	}
	switch c.Signal {
	case provider.SignalAuthFailure:
		g.ReportAuthFailure(name, c.Reason)
	case provider.SignalRateLimit:
		g.ReportRateLimit(name, c.RetryAfter, c.Reason, c.Source)
	case provider.SignalNone:
		// no-op: caller decided not to escalate this run.
	}
}

// DefaultProvider returns the current default provider name.
func (m *Manager) DefaultProvider() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultProv
}

// SetGuardrails configures cost and turn limits applied to all agents.
func (m *Manager) SetGuardrails(g Guardrails) {
	m.mu.Lock()
	m.guardrails = g
	m.mu.Unlock()
}

// Guardrails returns the current guardrail limits.
func (m *Manager) Guardrails() Guardrails {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.guardrails
}

// canAutoContinueTurns returns true when the agent's current cost is below
// TurnCostFraction * MaxCostUSD, meaning there is still meaningful budget left
// and the turns limit can be auto-bumped without human approval.
// If MaxCostUSD == 0, auto-continue is always allowed (cost is unlimited).
// Verifier roles (review/test-runner/eval, see Role.IsVerifier) never
// auto-continue: cost only updates on "result" events, so a fan-out can hold
// it at a stale $0 for the whole run, and a verifier stuck in a loop should
// escalate rather than silently get 2x/4x/8x the turn budget.
func (m *Manager) canAutoContinueTurns(a *Agent) bool {
	if a.EffectiveRole().IsVerifier() {
		return false
	}
	m.mu.RLock()
	maxCost := m.guardrails.MaxCostUSD
	fraction := m.guardrails.TurnCostFraction
	m.mu.RUnlock()
	if maxCost == 0 {
		return true
	}
	if fraction <= 0 {
		fraction = 0.8
	}
	return a.LiveCostEstimateUSD() < maxCost*fraction
}

// effectiveMaxSubagentEvents returns the configured forked-subagent turn
// ceiling (0 disables it).
func (m *Manager) effectiveMaxSubagentEvents() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.guardrails.MaxSubagentEvents
}

func (m *Manager) canCheckpointOnTurnCeiling(a *Agent) bool {
	if !a.EffectiveRole().AuthorsCode() {
		return false
	}
	// A read-only dispatch dir is diagnostic-only, and the checkpoint commit
	// runs in this process rather than in the sandboxed child — so nothing
	// else stops it writing there. On the deploy host that dir is the Sybra
	// source checkout, which is also auto_update.repo_dir: a commit lands
	// there permanently and breaks the ff-only merge auto-deploy relies on.
	if a.sessionReadOnly {
		return false
	}
	m.mu.RLock()
	enabled := m.guardrails.CheckpointOnTurnCeiling
	m.mu.RUnlock()
	return enabled
}

// effectiveTurnMultiplier returns the configured TurnMultiplier, defaulting to 2.
func (m *Manager) effectiveTurnMultiplier() float64 {
	m.mu.RLock()
	v := m.guardrails.TurnMultiplier
	m.mu.RUnlock()
	if v <= 0 {
		return 2
	}
	return v
}

// RespondEscalation sends a human decision to a paused agent.
// continueRun=true lets the agent keep running; false kills it.
func (m *Manager) RespondEscalation(agentID string, continueRun bool) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.escalationCh == nil {
		return fmt.Errorf("agent %s has no pending escalation", agentID)
	}
	select {
	case a.escalationCh <- continueRun:
	default:
		return fmt.Errorf("agent %s escalation channel full or closed", agentID)
	}
	return nil
}

// recordCompletion records duration + result into the metrics pipeline.
// Call through fireComplete — do not call directly from runner terminal sites.
//
// Duration is measured against the last observed stream activity, not against
// this call's own wall-clock time: fireComplete can run long after the
// process actually finished (reattach/stop recovering a run the app missed
// while it was down), and time.Since(a.StartedAt) would then count that idle
// gap as run time.
func (m *Manager) recordCompletion(ctx context.Context, a *Agent, ok bool) {
	dur := max(a.GetLastEventAt().Sub(a.StartedAt), 0)
	result := "ok"
	if !ok {
		result = "error"
	}
	metrics.AgentCompleted(ctx, result, dur)
}

// fireComplete records completion metrics and fires onComplete exactly once
// per agent. The guard prevents a second runner goroutine (e.g.
// runner_convo_survive whose tail is still live when runner_convo exits) from
// calling onComplete a second time and double-advancing the workflow.
func (m *Manager) fireComplete(ctx context.Context, a *Agent, ok bool) {
	a.completedOnce.Do(func() {
		// Release/finalize durable ownership before the callback advances a
		// workflow and synchronously admits its next mutating attempt on the
		// same worktree.
		m.completeAttempt(ctx, a, attemptTerminalOutcome(a))
		m.recordCompletion(ctx, a, ok)
		if m.onComplete != nil {
			m.onComplete(a)
		}
	})
}

// RunningCount returns the number of currently running agents.
func (m *Manager) RunningCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.liveCount
}

// TryHoldCapacity reserves one slot in the global agent pool until the caller
// releases it or converts it into a live agent registration.
func (m *Manager) TryHoldCapacity() (*CapacityReservation, bool) {
	return m.TryHoldCapacityWithLimit(0)
}

// TryHoldCapacityWithLimit reserves one slot in the global agent pool, but
// against a ceiling of min(maxConcurrent, extraLimit) instead of the raw pool
// cap. extraLimit <= 0 means "no extra ceiling" and behaves exactly like
// TryHoldCapacity. Both liveCount and reservedCount are counted under the lock,
// so a burst of concurrent callers cannot each observe a stale live-only count
// and collectively overshoot extraLimit — the SLO throttle relies on this to
// hold admission at the halved ceiling atomically.
//
// The reservation always counts against ClassImplementation — see
// CapacityReservation.class.
func (m *Manager) TryHoldCapacityWithLimit(extraLimit int) (*CapacityReservation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ceiling := m.maxConcurrent
	if extraLimit > 0 && (ceiling <= 0 || extraLimit < ceiling) {
		ceiling = extraLimit
	}
	if ceiling <= 0 {
		return &CapacityReservation{manager: m, class: ClassImplementation}, true
	}
	if !admitClass(ClassImplementation, m.liveByClass, m.reservedByClass, m.classFloors, ceiling) {
		return nil, false
	}
	m.reservedCount++
	m.reservedByClass[ClassImplementation]++
	return &CapacityReservation{manager: m, counted: true, class: ClassImplementation}, true
}

// TryReserveSlot is an advisory peek at capacity: it reports whether a slot
// is currently available (liveCount below maxConcurrent, or the cap
// disabled) once both running agents and held reservations are counted, but
// without claiming or mutating anything. registerRunningAgent remains the sole
// authoritative live-agent claim, and TryHoldCapacity is the mutation path for
// dispatchers that must actually own the slot across pre-run work.
func (m *Manager) TryReserveSlot() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.maxConcurrent <= 0 || m.liveCount+m.reservedCount < m.maxConcurrent
}

// AvailableQueueDrainSlots returns how many queued manual items can be drained
// immediately, bounded by the caller's current manual-queue depth. This is a
// read-only helper for the app-level queue drain: it never claims capacity,
// and registerRunningAgent still re-checks the cap authoritatively.
//
// The manual queue only ever holds implementation-role items (see
// enqueueManualStart), so this simulates repeated ClassImplementation
// admission via admitClass on a local copy of the live/reserved snapshot:
// class floors reserved for other classes can leave fewer slots available to
// implementation than the raw free count would suggest, and this must
// reflect that instead of over-reporting drainable slots.
func (m *Manager) AvailableQueueDrainSlots(queueDepth int) int {
	if queueDepth <= 0 {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.maxConcurrent <= 0 {
		return queueDepth
	}
	live := maps.Clone(m.liveByClass)
	n := 0
	for n < queueDepth && admitClass(ClassImplementation, live, m.reservedByClass, m.classFloors, m.maxConcurrent) {
		live[ClassImplementation]++
		n++
	}
	return n
}

// signalQueueNudge fires a non-blocking, coalescing signal on queueNudge. If
// a nudge is already pending (buffer full), the send is dropped — one
// pending nudge is sufficient to prompt a dispatcher to re-scan.
func (m *Manager) signalQueueNudge() {
	if m.queueNudge == nil {
		return
	}
	select {
	case m.queueNudge <- struct{}{}:
	default:
	}
}

// QueueNudge returns a channel that receives a signal each time a slot frees
// (see markAgentDone), so an external dispatch loop can react promptly
// instead of waiting for its next poll tick. Mirrors App.dispatchNudge's
// buffer-1 coalescing contract.
func (m *Manager) QueueNudge() <-chan struct{} {
	return m.queueNudge
}

// SetToolLedger late-binds the tool-call ledger. Separate from construction
// because the ledger's directory is resolved from config the Manager does not
// own.
func (m *Manager) SetToolLedger(l toolledger.Store) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.toolLedger = l
	m.mu.Unlock()
}

// ToolLedger reports the bound ledger. Exported because a nil ledger is
// otherwise invisible: Logger.Log guards its own nil receiver, so an unwired
// manager drops every record without erroring, and only a direct read of the
// binding can tell a live ledger from a silent one.
func (m *Manager) ToolLedger() toolledger.Store {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.toolLedger
}

// SetBoard names this instance's own board, so a task-scoped agent's sybra-cli
// is told where to go rather than left to infer it.
//
// Inference does not survive the process sandbox: reads are deny-by-default
// against an allowlist, so the port file the CLI would discover a board from is
// not reliably readable from inside a run. The address is only known once this
// instance is listening, which is after the manager is built, so it is set
// rather than configured.
// SetBoard names the board every task-scoped agent's sybra-cli talks to.
//
// ca is the path to the board's certificate, and is required when target is an
// https origin: a board serving TLS signs its own certificate, so a client that
// cannot read that file has no way to verify it. Empty for a cleartext board.
//
// Call it before any agent can be dispatched. A run that starts before this
// lands gets no target at all, and every CLI call it makes refuses — which is a
// whole paid run producing nothing.
func (m *Manager) SetBoard(target, token, ca string) {
	m.boardMu.Lock()
	defer m.boardMu.Unlock()
	m.boardTarget, m.boardToken, m.boardCA = target, token, ca
}

func (m *Manager) board() (target, token, ca string) {
	m.boardMu.RLock()
	defer m.boardMu.RUnlock()
	return m.boardTarget, m.boardToken, m.boardCA
}

// boardTokenFile and boardCAFile are the credentials an agent's CLI reads from
// its own sandbox home, which is the only directory it is guaranteed to be
// able to read under an enforcing sandbox.
const (
	boardTokenFile = "board-token"
	boardCAFile    = "board-ca.pem"
)
