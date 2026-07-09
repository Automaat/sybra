package agent

import (
	"context"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// NOTE on concurrency: Agent has distinct mutexes.
//   - mu: guards all mutable scalar and slice fields (State, outputBuffer,
//     cmd, PID, LogPath, EscalationReason, and the non-stdin fields in
//     convoIO). Use the helper methods defined on Agent rather than touching
//     the fields directly from concurrent code paths.
//   - convo.stdinMu: guards convo.stdinPipe only. Kept separate because the
//     runner goroutine may hold it for the duration of a blocking Write, and
//     we do not want to starve other consumers that only need to read State or
//     append an event.
//   - loops owns its own lock for loop-detection state. Runner write paths and
//     watchdog read/ack paths intentionally observe loop state independently
//     from Agent.mu-protected fields; do not assume atomic snapshots across
//     those lock domains.

type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
)

const DefaultReasoningEffort = "medium"

type Agent struct {
	ID                       string  `json:"id"`
	TaskID                   string  `json:"taskId"`
	Mode                     string  `json:"mode"`
	State                    State   `json:"state"`
	SessionID                string  `json:"sessionId"`
	CostUSD                  float64 `json:"costUsd"`
	InputTokens              int     `json:"inputTokens,omitempty"`
	OutputTokens             int     `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int     `json:"reasoningTokens,omitempty"`
	// PremiumRequests is Copilot's billing unit (AI credits). Sybra keeps the
	// raw count alongside the estimated USD equivalent persisted on task runs.
	PremiumRequests float64   `json:"premiumRequests,omitempty"`
	StartedAt       time.Time `json:"startedAt"`
	LastEventAt     time.Time `json:"lastEventAt"`
	LogPath         string    `json:"logPath,omitempty"`
	External        bool      `json:"external"`
	PID             int       `json:"pid,omitempty"`
	Command         string    `json:"command,omitempty"`
	Name            string    `json:"name,omitempty"`
	Project         string    `json:"project,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	Model           string    `json:"model,omitempty"`
	ExperimentID    string    `json:"experimentId,omitempty"`
	VariantID       string    `json:"variantId,omitempty"`
	AssignmentUnit  string    `json:"assignmentUnit,omitempty"`
	AssignmentKey   string    `json:"assignmentKey,omitempty"`
	ReasoningEffort string    `json:"reasoningEffort,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`

	TurnCount int `json:"turnCount,omitempty"`
	// ToolCalls counts tool_use blocks observed across the run. Persisted to
	// stats.RunRecord at completion so efficiency (tools per turn, tools per
	// landed PR) can be measured. Tracked in-memory during the run.
	ToolCalls int `json:"toolCalls,omitempty"`
	loops     loopDetector
	// MaxTurns is the per-agent turn limit override; zero means use global guardrail.
	MaxTurns int `json:"maxTurns,omitempty"`
	// oneShot marks workflow-owned interactive runs that must complete after
	// one provider turn instead of surviving as reusable chats.
	oneShot bool
	// PluginErrors holds plugin load failures from the most recent init event.
	PluginErrors     []string `json:"pluginErrors,omitempty"`
	EscalationReason string   `json:"escalationReason,omitempty"`
	ErrorKind        string   `json:"errorKind,omitempty"`
	ErrorMsg         string   `json:"errorMsg,omitempty"`
	AwaitingApproval bool     `json:"awaitingApproval,omitempty"`
	// Resumable is set when the agent was stopped intentionally via StopAgent
	// and CC exited with a valid session_id, meaning the next run can pass
	// --resume to continue the conversation.
	Resumable bool `json:"resumable,omitempty"`

	ExitErr         error `json:"-"`
	outputBuffer    []StreamEvent
	convoBuffer     []ConvoEvent
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	sessionCWD      string
	sessionFilePath string // path to provider session file (Codex JSONL)
	// done is closed when the headless/conversational goroutine has fully exited.
	// Used by HasRunningAgentForTask to guard worktree cleanup.
	done chan struct{}
	// doneOnce prevents double-close on done and lets Manager maintain an
	// exact live-agent count even when multiple terminal paths race.
	doneOnce sync.Once
	// completedOnce guards recordCompletion + onComplete so that two runner
	// goroutines reaching their terminal sites for the same agent (e.g.
	// runner_convo and runner_convo_survive both firing when the process exits
	// while a reattach tail is still live) only advance the workflow once.
	completedOnce sync.Once
	// costSessionID and costBaseUSD back AddResultStats' per-session cost
	// bookkeeping. Providers report CostUSD as a cumulative total for the
	// current session (not a per-turn delta), so a repeated session id must
	// replace the running total rather than add to it; costBaseUSD banks the
	// last cumulative snapshot of any prior session once a new session id
	// appears (e.g. across a --resume segment boundary).
	costSessionID string
	costBaseUSD   float64

	// escalationCh receives the human's decision when a guardrail is hit.
	// true = continue, false = kill.
	escalationCh chan bool

	convo convoIO

	// stopped is set by StopAgent before cancelling the context so
	// OnComplete can distinguish an intentional user stop (SIGTERM via
	// cancel) from an infra-level kill (OS/container SIGTERM). Both exit
	// with a signal, but only intentional stops should advance the
	// workflow step to "failed".
	stopped bool

	// completedByResult is set when the post-result-hang guard stopped a
	// headless process that emitted a terminal (non-error) result but never
	// exited on its own. It tells the finalize path to derive completion
	// status from the result event rather than the kill signal.
	completedByResult bool

	// backgroundTaskIDs mirrors the CLI's last "background_tasks_changed"
	// system event (REPLACE semantics: the full set of currently-live
	// background bash tasks, e.g. a `run_in_background` Bash call). A CC
	// process can legitimately emit its terminal result while a task like
	// `npm ci` is still running in the background, producing no further
	// NDJSON activity — the post-result-hang guards must not mistake that
	// silence for a hung/orphaned process and kill it mid-write (task
	// 3aeabb65: a killed `npm ci` left a corrupted, zero-byte node_modules).
	backgroundTaskIDs []string

	// detached is true when the agent's subprocess was spawned to survive
	// an app restart (Setsid, output redirected to its log file, no ctx
	// kill). ShutdownWithGrace leaves detached agents running instead of
	// cancelling them. Guarded by mu.
	detached bool

	// requirePermissions mirrors RunConfig.RequirePermissions. Persisted to
	// the registry so a recreated codex chat keeps its sandbox/approval
	// choice across a restart instead of silently becoming permissive.
	requirePermissions bool

	// sandboxMode mirrors the resolved RunConfig.SandboxMode. Persisted to the
	// registry so a recreated per-turn conversational chat preserves its OS
	// process-sandbox posture across restart instead of silently dropping an
	// enforce-mode seatbelt.
	sandboxMode string

	// headlessPermissionMode is the resolved posture passed via RunConfig
	// ("bypass" or "auto"). Stored for OnComplete so the denial audit events
	// can record the posture without re-resolving it.
	headlessPermissionMode string

	// permissionDenials accumulates auto-mode classifier denial records
	// observed during the run. Flushed to audit in OnComplete.
	permissionDenials []PermissionDenial

	// handoff is set by SendMessage/regateBeforeClaudeTurn when a persistent
	// Claude interactive agent's provider is switched at a turn boundary. The
	// still-idle Claude process is torn down (closeStdinPipe/signalKill); once
	// runConversational's goroutine observes the process actually exit, it
	// consumes this instead of finalizing, and hands the same *Agent off to
	// runPerTurnConversational on the new provider.
	handoff *pendingConvoHandoff

	// mu guards mutable fields touched from multiple goroutines. See the
	// package-level note above the Agent type.
	mu sync.RWMutex
}

// pendingConvoHandoff carries the RunConfig and next prompt for a mid-run
// persistent-Claude -> per-turn provider switch. See Agent.handoff.
type pendingConvoHandoff struct {
	cfg    RunConfig
	prompt string
}

// SetPendingHandoff records a same-agent provider switch to be picked up by
// runConversational's finalize path once its (now-doomed) process exits.
func (a *Agent) SetPendingHandoff(cfg RunConfig, prompt string) {
	a.mu.Lock()
	a.handoff = &pendingConvoHandoff{cfg: cfg, prompt: prompt}
	a.mu.Unlock()
}

// ConsumePendingHandoff returns and clears any pending handoff recorded by
// SetPendingHandoff.
func (a *Agent) ConsumePendingHandoff() (RunConfig, string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handoff == nil {
		return RunConfig{}, "", false
	}
	h := a.handoff
	a.handoff = nil
	return h.cfg, h.prompt, true
}

// toRecord snapshots only the fields persisted for restart survival.
// Callers that write a live process record must fill ProcStartedAt after the
// snapshot so the PID-reuse guard reflects the current process state.
func (a *Agent) toRecord() Record {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return Record{
		ID:                 a.ID,
		TaskID:             a.TaskID,
		Name:               a.Name,
		Mode:               a.Mode,
		Provider:           a.Provider,
		Model:              a.Model,
		ExperimentID:       a.ExperimentID,
		VariantID:          a.VariantID,
		AssignmentUnit:     a.AssignmentUnit,
		AssignmentKey:      a.AssignmentKey,
		PID:                a.PID,
		SessionID:          a.SessionID,
		LogPath:            a.LogPath,
		CWD:                a.sessionCWD,
		StartedAt:          a.StartedAt,
		StdinPath:          a.convo.stdinPath,
		OneShot:            a.oneShot,
		MaxTurns:           a.MaxTurns,
		RequirePermissions: a.requirePermissions,
		SandboxMode:        a.sandboxMode,
		ReasoningEffort:    a.ReasoningEffort,
	}
}

// fromRecord builds a detached reattach skeleton, not a general Agent factory.
// Reattach callers own runtime wiring such as cancel, done, cmd, and promptCh.
func fromRecord(r Record) *Agent {
	return &Agent{
		ID:                 r.ID,
		TaskID:             r.TaskID,
		Name:               r.Name,
		Mode:               r.Mode,
		Provider:           r.Provider,
		Model:              r.Model,
		ExperimentID:       r.ExperimentID,
		VariantID:          r.VariantID,
		AssignmentUnit:     r.AssignmentUnit,
		AssignmentKey:      r.AssignmentKey,
		PID:                r.PID,
		SessionID:          r.SessionID,
		LogPath:            r.LogPath,
		sessionCWD:         r.CWD,
		StartedAt:          r.StartedAt,
		LastEventAt:        time.Now().UTC(),
		State:              StateRunning,
		MaxTurns:           r.MaxTurns,
		oneShot:            r.OneShot,
		convo:              convoIO{stdinPath: r.StdinPath},
		requirePermissions: r.RequirePermissions,
		sandboxMode:        r.SandboxMode,
		ReasoningEffort:    r.ReasoningEffort,
		detached:           true,
	}
}

// SetState atomically updates the agent state.
func (a *Agent) SetState(s State) {
	a.mu.Lock()
	a.State = s
	a.mu.Unlock()
}

// SetAwaitingApproval marks whether the agent is paused pending tool approval.
func (a *Agent) SetAwaitingApproval(v bool) {
	a.mu.Lock()
	a.AwaitingApproval = v
	a.mu.Unlock()
}

// GetState returns the agent's current state.
func (a *Agent) GetState() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.State
}

// MarkStopped records that the agent was stopped intentionally via StopAgent.
func (a *Agent) MarkStopped() {
	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()
}

// WasStopped reports whether StopAgent was called on this agent.
func (a *Agent) WasStopped() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stopped
}

// AppendOutput appends a stream event to the headless output buffer and
// refreshes LastEventAt.
func (a *Agent) AppendOutput(ev StreamEvent) {
	a.mu.Lock()
	a.outputBuffer = append(a.outputBuffer, ev)
	a.LastEventAt = time.Now().UTC()
	a.mu.Unlock()
}

// AppendConvo appends a conversational event and refreshes LastEventAt.
func (a *Agent) AppendConvo(ev ConvoEvent) {
	a.mu.Lock()
	a.convoBuffer = append(a.convoBuffer, ev)
	a.LastEventAt = time.Now().UTC()
	a.mu.Unlock()
}

// SetCmd records the running process and its PID.
func (a *Agent) SetCmd(cmd *exec.Cmd) {
	a.mu.Lock()
	a.cmd = cmd
	if cmd != nil && cmd.Process != nil {
		a.PID = cmd.Process.Pid
	}
	a.mu.Unlock()
}

// GetCmd returns the agent's current command (nil if not yet started or already reaped).
func (a *Agent) GetCmd() *exec.Cmd {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cmd
}

// SetResumable marks whether this agent's CC session can be resumed via --resume.
func (a *Agent) SetResumable(v bool) {
	a.mu.Lock()
	a.Resumable = v
	a.mu.Unlock()
}

// IsResumable reports whether this agent's session can be resumed.
func (a *Agent) IsResumable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Resumable
}

// SetExitErr records the exit error of the underlying process.
func (a *Agent) SetExitErr(err error) {
	a.mu.Lock()
	a.ExitErr = err
	a.mu.Unlock()
}

// GetExitErr returns the recorded exit error, if any.
func (a *Agent) GetExitErr() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExitErr
}

// SetLogPath records the path of the agent's output log file.
func (a *Agent) SetLogPath(p string) {
	a.mu.Lock()
	a.LogPath = p
	a.mu.Unlock()
}

// SetSessionID records the provider session ID.
func (a *Agent) SetSessionID(id string) {
	a.mu.Lock()
	a.SessionID = id
	a.mu.Unlock()
}

// GetSessionID returns the current provider session ID.
func (a *Agent) GetSessionID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SessionID
}

// GetStartedAt returns the agent's recorded start time.
func (a *Agent) GetStartedAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.StartedAt
}

// SetProviderAndModel updates the agent's provider and (already-normalized)
// model. Used when a mid-run per-turn provider re-gate fails the agent's
// current provider over to a healthy peer; Provider/Model are otherwise fixed
// for the lifetime of the agent (set once at construction).
func (a *Agent) SetProviderAndModel(prov, model string) {
	a.mu.Lock()
	a.Provider = prov
	a.Model = model
	a.mu.Unlock()
}

// GetProvider returns the agent's current provider name. Safe to call
// concurrently with a mid-run SetProviderAndModel switch; code within the
// single-threaded runner loop (which owns all writes) may keep reading
// a.Provider directly since it's the same goroutine, but any external
// reader must go through this to avoid racing the switch.
func (a *Agent) GetProvider() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Provider
}

// SetSessionFilePath records the path to the provider session file.
func (a *Agent) SetSessionFilePath(p string) {
	a.mu.Lock()
	a.sessionFilePath = p
	a.mu.Unlock()
}

// GetSessionFilePath returns the provider session file path.
func (a *Agent) GetSessionFilePath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sessionFilePath
}

// AddResultStats merges a result-event's stats into the running totals and
// returns the new cumulative CostUSD. Token counts are genuine per-turn
// deltas and accumulate normally. Cost is not: providers report CostUSD as
// the cumulative total for the whole session (including every prior turn,
// and — across a --resume segment boundary — every prior segment too), so
// summing it turn over turn multiplies the true spend several times over.
// A same-session event therefore replaces the running total instead of
// adding to it; only a change in session id banks the previous session's
// final snapshot and starts counting the new one from there.
func (a *Agent) AddResultStats(sessionID string, cost float64, in, out, reasoning int) float64 {
	cost = sanitizeCostUSD(cost)
	a.mu.Lock()
	if sessionID != "" {
		a.SessionID = sessionID
	}
	if sessionID != "" && sessionID != a.costSessionID {
		a.costBaseUSD = a.CostUSD
		a.costSessionID = sessionID
	}
	a.CostUSD = a.costBaseUSD + cost
	a.InputTokens += in
	a.OutputTokens += out
	a.ReasoningTokens += reasoning
	result := a.CostUSD
	a.mu.Unlock()
	return result
}

// AddCacheStats merges cache token counts into the running totals.
// Kept separate from AddResultStats so the existing 5-arg signature stays
// stable across runners.
func (a *Agent) AddCacheStats(cacheCreate, cacheRead int) {
	a.mu.Lock()
	a.CacheCreationInputTokens += cacheCreate
	a.CacheReadInputTokens += cacheRead
	a.mu.Unlock()
}

// AddPremiumRequests merges Copilot premium-request usage into the totals.
// Copilot reports usage in premium requests (AI credits) rather than USD;
// claude/codex never call this (their result events carry no such field).
func (a *Agent) AddPremiumRequests(n float64) {
	a.mu.Lock()
	a.PremiumRequests += n
	a.mu.Unlock()
}

// AddOutputTokens merges per-message output tokens into the totals. Copilot
// reports output tokens on each assistant.message rather than once on the
// terminal result, so the headless runner accumulates them here as they
// stream. No-op-equivalent for claude/codex, whose assistant events carry 0.
func (a *Agent) AddOutputTokens(n int) {
	a.mu.Lock()
	a.OutputTokens += n
	a.mu.Unlock()
}

// GetPremiumRequests returns the cumulative Copilot premium-request count.
func (a *Agent) GetPremiumRequests() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PremiumRequests
}

// EnqueuePrompt appends a follow-up prompt to the pending queue.
func (a *Agent) EnqueuePrompt(text string) {
	a.mu.Lock()
	a.convo.pendingPrompts = append(a.convo.pendingPrompts, text)
	a.mu.Unlock()
}

// PopPendingPrompt returns the next queued prompt and a flag indicating
// whether a value was popped.
func (a *Agent) PopPendingPrompt() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.convo.pendingPrompts) == 0 {
		return "", false
	}
	next := a.convo.pendingPrompts[0]
	a.convo.pendingPrompts = a.convo.pendingPrompts[1:]
	return next, true
}

// PendingPromptCount returns the size of the pending prompt queue.
func (a *Agent) PendingPromptCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.convo.pendingPrompts)
}

// RestorePendingPrompt pushes text back onto the front of the pending queue.
// Used by the turn-boundary chokepoint (advanceClaudeTurn) to put a
// just-reserved prompt back where it came from when the turn cannot proceed
// (no healthy peer, write failure, cancellation), so it is retried in order
// rather than lost or reordered behind prompts queued afterward.
func (a *Agent) RestorePendingPrompt(text string) {
	a.mu.Lock()
	a.convo.pendingPrompts = append([]string{text}, a.convo.pendingPrompts...)
	a.mu.Unlock()
}

// IncTurnCount increments the turn counter and returns the new value.
func (a *Agent) IncTurnCount() int {
	a.mu.Lock()
	a.TurnCount++
	n := a.TurnCount
	a.mu.Unlock()
	return n
}

// GetTurnCount returns the current turn count.
func (a *Agent) GetTurnCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.TurnCount
}

// AddToolCalls increments the tool-call counter by n. No-op for n <= 0.
func (a *Agent) AddToolCalls(n int) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.ToolCalls += n
	a.mu.Unlock()
}

// GetToolCalls returns the number of tool_use blocks observed so far.
func (a *Agent) GetToolCalls() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ToolCalls
}

// NoteToolSignature feeds the next assistant event's tool-call signature into
// the loop detector and returns the resulting consecutive-repeat streak. An
// empty signature (an assistant turn with no tool calls — pure text/thinking)
// carries no loop signal and leaves the streak untouched, so interleaved
// reasoning between identical calls does not reset a genuine loop. A new
// signature resets the streak to 1.
func (a *Agent) NoteToolSignature(sig string) int {
	return a.loops.noteSignature(sig)
}

// ToolLoopStreak returns the current count of consecutive identical tool-call
// signatures. A high value means the agent is repeating the same call.
func (a *Agent) ToolLoopStreak() int {
	return a.loops.currentStreak()
}

// AckToolLoop records the current loop signature as already inspected, so the
// watchdog does not re-trigger on the same unchanged loop. Called after a
// loop-triggered inspection whose verdict left the agent running.
func (a *Agent) AckToolLoop() {
	a.loops.ack()
}

// ToolLoopAcknowledged reports whether the current loop signature has already
// been inspected (and the agent left running). True suppresses the loop
// trigger until the signature changes.
func (a *Agent) ToolLoopAcknowledged() bool {
	return a.loops.acknowledged()
}

// SetEscalationReason updates the escalation reason string.
func (a *Agent) SetEscalationReason(reason string) {
	a.mu.Lock()
	a.EscalationReason = reason
	a.mu.Unlock()
}

// GetEscalationReason returns the current guardrail escalation reason.
func (a *Agent) GetEscalationReason() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.EscalationReason
}

// NotePermissionDenial records an auto-mode classifier denial observed during the run.
func (a *Agent) NotePermissionDenial(toolUseID, reason string) {
	a.mu.Lock()
	a.permissionDenials = append(a.permissionDenials, PermissionDenial{
		ToolUseID: toolUseID,
		Reason:    reason,
	})
	a.mu.Unlock()
}

// GetPermissionDenials returns a snapshot of the recorded auto-mode denial records.
func (a *Agent) GetPermissionDenials() []PermissionDenial {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.permissionDenials) == 0 {
		return nil
	}
	cp := make([]PermissionDenial, len(a.permissionDenials))
	copy(cp, a.permissionDenials)
	return cp
}

// GetHeadlessPermissionMode returns the resolved headless permission posture for this run.
func (a *Agent) GetHeadlessPermissionMode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.headlessPermissionMode
}

// SetPluginErrors records plugin load failures from the init event.
func (a *Agent) SetPluginErrors(errs []string) {
	a.mu.Lock()
	cp := make([]string, len(errs))
	copy(cp, errs)
	a.PluginErrors = cp
	a.mu.Unlock()
}

// GetPluginErrors returns a snapshot of the recorded plugin errors.
func (a *Agent) GetPluginErrors() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.PluginErrors) == 0 {
		return nil
	}
	cp := make([]string, len(a.PluginErrors))
	copy(cp, a.PluginErrors)
	return cp
}

// SetMaxTurns sets the per-agent turn limit override.
func (a *Agent) SetMaxTurns(n int) {
	a.mu.Lock()
	a.MaxTurns = n
	a.mu.Unlock()
}

// GetCostUSD returns the current cumulative cost.
func (a *Agent) GetCostUSD() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CostUSD
}

// GetInputTokens returns the cumulative input token count.
func (a *Agent) GetInputTokens() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.InputTokens
}

// GetCacheCreationInputTokens returns the cumulative cache-creation input tokens.
// For Claude these are billed at the cache-write rate (typically 1.25× standard
// input). For Codex this is always 0 — Codex only reports a cached subset of
// gross input.
func (a *Agent) GetCacheCreationInputTokens() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CacheCreationInputTokens
}

// GetCacheReadInputTokens returns cumulative cache-read input tokens.
// Claude bills these at ~10% of standard input. For Codex this is the
// `cached_input_tokens` subset of gross input_tokens.
func (a *Agent) GetCacheReadInputTokens() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CacheReadInputTokens
}

// GetOutputTokens returns the cumulative output token count.
func (a *Agent) GetOutputTokens() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.OutputTokens
}

// GetReasoningTokens returns the cumulative reasoning token count.
func (a *Agent) GetReasoningTokens() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ReasoningTokens
}

// GetLogPath returns the current output log path.
func (a *Agent) GetLogPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LogPath
}

// GetPID returns the subprocess PID (0 if not yet started).
func (a *Agent) GetPID() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.PID
}

// setStdinPath records the FIFO path backing a detached conversational
// agent's stdin.
func (a *Agent) setStdinPath(p string) {
	a.mu.Lock()
	a.convo.stdinPath = p
	a.mu.Unlock()
}

// GetStdinPath returns the FIFO path backing the agent's stdin ("" for
// pipe-backed agents).
func (a *Agent) GetStdinPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.convo.stdinPath
}

func (a *Agent) setPromptChannel(ch chan string) {
	a.mu.Lock()
	a.convo.promptCh = ch
	a.mu.Unlock()
}

func (a *Agent) promptChannel() chan string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.convo.promptCh
}

func (a *Agent) hasPromptChannel() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.convo.promptCh != nil
}

// setDetached marks whether the agent's subprocess is detached for
// restart survival.
func (a *Agent) setDetached(v bool) {
	a.mu.Lock()
	a.detached = v
	a.mu.Unlock()
}

// isDetached reports whether the agent's subprocess was spawned to
// survive an app restart.
func (a *Agent) isDetached() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.detached
}

// lastHeadlessResult reports whether the output buffer contains a terminal
// result event and whether that result was a provider error. Used by reattach
// completion to distinguish a clean finish from an error result or a process
// that vanished mid-run.
func (a *Agent) lastHeadlessResult() (found, isError bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return lastHeadlessResultEvent(a.outputBuffer)
}

func lastHeadlessResultEvent(events []StreamEvent) (found, isError bool) {
	if len(events) == 0 {
		return false, false
	}
	last := events[len(events)-1]
	found = last.Type == "result"
	if !found {
		return false, false
	}
	return true, resultSubtypeIsError(last.Subtype) || last.ErrorType != "" || last.ErrorStatus != 0
}

// lastConvoResult reports whether a terminal result event was observed in
// the conversational buffer and whether that result was an error, scanning
// newest-first. Used by reattach completion to tell a clean finish from an
// error completion from a process that vanished mid-turn.
func (a *Agent) lastConvoResult() (found, isError bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for i := range slices.Backward(a.convoBuffer) {
		if a.convoBuffer[i].Type == "result" {
			e := a.convoBuffer[i]
			return true, resultSubtypeIsError(e.Subtype) || e.ErrorType != "" || e.ErrorStatus != 0
		}
	}
	return false, false
}

// CompletedSuccessfully reports whether the headless agent's stream buffer
// ends in a non-error terminal result. The watchdog uses it to skip
// inspecting (and never escalate) an agent that has already produced its
// final result but whose process has not yet exited — a skill that spawns
// subagents can leave CC alive after the terminal result.
func (a *Agent) CompletedSuccessfully() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	found, isError := lastHeadlessResultEvent(a.outputBuffer)
	return found && !isError
}

// TerminalResultIdle reports whether the headless stream's last event is a
// non-error terminal result and no further output has arrived for at least
// grace. It flags a process that finished its work (emitted the final result)
// but did not exit, so the runner can stop it and finalize from the result.
func (a *Agent) TerminalResultIdle(grace time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	found, isError := lastHeadlessResultEvent(a.outputBuffer)
	if !found || isError {
		return false
	}
	return time.Since(a.LastEventAt) >= grace
}

// backgroundTaskGrace is the extra idle time granted to a post-result-hang
// guard (runner_headless.go's postResultGrace, watchdog's completedHangGrace)
// while the agent has outstanding CLI background bash tasks. Bounded rather
// than unlimited so a task that never reports completion (e.g. the CLI
// process dies without emitting a final background_tasks_changed) can't hang
// a run forever — the guard still fires, just later, once this is exhausted.
const backgroundTaskGrace = 15 * time.Minute

// SetBackgroundTaskIDs replaces the agent's tracked set of live CLI
// background bash tasks, mirroring the REPLACE semantics of the CLI's
// "background_tasks_changed" system event.
func (a *Agent) SetBackgroundTaskIDs(ids []string) {
	// Defensive copy, mirroring SetPluginErrors, so a caller that later mutates
	// or reuses the passed slice can't race the reader in HasBackgroundTasks.
	// Preserve nil-ness: a nil "no event seen" snapshot must stay distinct from
	// an empty non-nil "all tasks cleared" one (REPLACE semantics).
	var cp []string
	if ids != nil {
		cp = make([]string, len(ids))
		copy(cp, ids)
	}
	a.mu.Lock()
	a.backgroundTaskIDs = cp
	a.mu.Unlock()
}

// HasBackgroundTasks reports whether the CLI last reported any live
// background bash tasks still running.
func (a *Agent) HasBackgroundTasks() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.backgroundTaskIDs) > 0
}

// EffectiveHangGrace extends base by backgroundTaskGrace while the agent has
// outstanding background bash tasks, so a post-result-hang guard idle-timing
// out on silence doesn't kill a process that's still legitimately waiting on
// a `run_in_background` command (e.g. npm ci) — see backgroundTaskIDs.
func (a *Agent) EffectiveHangGrace(base time.Duration) time.Duration {
	if a.HasBackgroundTasks() {
		return base + backgroundTaskGrace
	}
	return base
}

func (a *Agent) setCompletedByResult(v bool) {
	a.mu.Lock()
	a.completedByResult = v
	a.mu.Unlock()
}

func (a *Agent) wasCompletedByResult() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.completedByResult
}

// WasCompletedByResult reports whether the agent's completion was derived
// from a clean terminal result event (via StopCompletedAgent or the runner's
// own post-result-hang reaper) rather than an intentional mid-run stop. A
// caller must not treat such an agent as stalled merely because WasStopped()
// is also true — StopCompletedAgent marks both flags, since force-stopping
// the now-orphaned process is still implemented via StopAgent.
func (a *Agent) WasCompletedByResult() bool {
	return a.wasCompletedByResult()
}

// GetLastEventAt returns the most recent event timestamp.
func (a *Agent) GetLastEventAt() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LastEventAt
}

// TouchLastEvent refreshes LastEventAt without appending any event.
// Used by the stdout reader to keep the stall clock alive during
// extended thinking, where no complete NDJSON lines are emitted.
func (a *Agent) TouchLastEvent() {
	a.mu.Lock()
	a.LastEventAt = time.Now().UTC()
	a.mu.Unlock()
}

// SetLastEventAt overrides the last-activity timestamp directly. Used by
// dead-process reattach recovery, where every replayed event is stamped at
// replay time (not history), so AppendOutput/AppendConvo would otherwise
// collapse LastEventAt to the wall-clock moment finalization happens to run.
func (a *Agent) SetLastEventAt(t time.Time) {
	a.mu.Lock()
	a.LastEventAt = t
	a.mu.Unlock()
}

// Output returns a snapshot of the stream events produced so far. The
// returned slice is safe to inspect concurrently with the agent's runner
// goroutine appending more events.
func (a *Agent) Output() []StreamEvent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snapshot := make([]StreamEvent, len(a.outputBuffer))
	copy(snapshot, a.outputBuffer)
	return snapshot
}

// RunConfig is the single entry point for starting any agent.
type RunConfig struct {
	TaskID             string
	Name               string
	Mode               string // "headless", "interactive", or "conversational"
	Prompt             string
	AllowedTools       []string
	Dir                string
	Provider           string // "claude", "codex", or "copilot"
	Model              string // "opus", "sonnet", or full model ID
	ExperimentID       string
	VariantID          string
	AssignmentUnit     string
	AssignmentKey      string
	RequirePermissions bool   // when true, suppress --dangerously-skip-permissions
	PermissionMode     string // "default", "acceptEdits", "bypassPermissions" (conversational mode)
	// OneShot closes stdin after the first `result` event in conversational
	// mode so the claude process exits naturally. Without this, interactive
	// agents sit in StatePaused forever and onComplete never fires, stranding
	// any workflow that expects the agent to "finish". Ignored in headless mode.
	OneShot bool
	// IgnoreConcurrencyLimit lets an agent start even when MaxConcurrent is
	// saturated. Reserved for system-level long-lived sessions (orchestrator)
	// that must always be runnable regardless of swarm load.
	IgnoreConcurrencyLimit bool
	// IgnoreHealthGate lets an agent start even when the provider health gate
	// marks the requested provider as unhealthy. Reserved for internal probes
	// and system-critical sessions; user-initiated runs leave this false so
	// they surface a clear error instead of wasting a hopeless request.
	IgnoreHealthGate bool
	// DisableProviderFailover keeps provider selection fixed for A/B variants:
	// an unhealthy/limited provider fails the run instead of silently becoming a
	// different provider while retaining stale variant attribution.
	DisableProviderFailover bool
	// ResumeSessionID, when set, passes --resume to the claude CLI so the
	// agent continues a prior conversation instead of starting from scratch.
	// Populated from the task's last AgentRun.SessionID on restart.
	ResumeSessionID string
	// ExtraEnv is a list of "KEY=VALUE" strings appended to the subprocess
	// environment. Used to inject sandbox credentials (SANDBOX_URL, KUBECONFIG)
	// and, for every task-scoped run, the trusted SYBRA_HOME/SYBRA_CONTROL_HOME
	// pair that Manager.prepareRunConfig appends last (see ManagerConfig.SandboxHome) —
	// any caller-supplied entry for those two keys is stripped before the
	// trusted values are appended, so it cannot override them.
	ExtraEnv []string
	// MaxTurns overrides the global guardrail for this specific agent run.
	// Zero means "use the manager's global guardrail".
	MaxTurns int
	// BashTimeoutMs sets the Bash tool timeout for this run by exporting
	// BASH_DEFAULT_TIMEOUT_MS and BASH_MAX_TIMEOUT_MS into the claude
	// subprocess (claude exposes no equivalent CLI flag). Zero means "use
	// the manager's default".
	BashTimeoutMs int
	// ForkSubagent, when true, sets CLAUDE_CODE_FORK_SUBAGENT=1 in the claude
	// subprocess environment (claude provider only). Enables parallel subagent
	// spawning from a single prompt at the cost of higher token usage.
	ForkSubagent bool
	// RetryWatchdog, when > 0, sets CLAUDE_CODE_RETRY_WATCHDOG to this value
	// in the claude subprocess environment. Replaces CLAUDE_CODE_MAX_RETRIES
	// (now capped at 15) for headless/unattended server runs. Zero means "use
	// the manager's default".
	RetryWatchdog int
	// FallbackModel, when non-empty, passes --fallback-model to claude.
	// Paired with RetryWatchdog so the watchdog can retry on a less-loaded
	// model when the primary is overloaded. Empty means inherit the manager's
	// default; the flag is omitted only when the manager default is also empty.
	FallbackModel string
	// ReasoningEffort sets the agent's reasoning effort for this run
	// (low/medium/high/xhigh). Empty is resolved to DefaultReasoningEffort by
	// Manager.Run for every provider before command construction. Lower-level
	// command builders still omit the provider flag when handed an empty value
	// directly. Codex uses `-c model_reasoning_effort=`; claude and copilot use
	// `--effort`.
	ReasoningEffort string
	// SeedWorkingMemory, when true, inlines the worktree's NOTES.md scratchpad
	// into the prompt (read/maintain instruction + current contents). Set only
	// for code-author roles (see Role.AuthorsCode): verifier roles share the
	// implementation worktree, so seeding them would feed an independent
	// reviewer/tester the implementer's notes. No-op if the dir has no NOTES.md.
	SeedWorkingMemory bool
	// OutputSchema is a JSON Schema string enforcing the shape of the agent's
	// final response. Empty = no schema enforcement. Delivery differs by
	// provider (see Provider.OutputSchemaAsFile): codex receives it as a temp
	// file path via --output-schema <path> (the runner writes OutputSchema to
	// disk before invocation); claude receives it inline via
	// --json-schema <schema>. Ignored by copilot.
	OutputSchema string
	// outputSchemaPath is the temp file path the runner wrote OutputSchema to,
	// for providers where Provider.OutputSchemaAsFile() is true. Set
	// intra-package before buildHeadlessInvocation; cleared by defer after the
	// subprocess exits. Never set by callers.
	outputSchemaPath string
	// HeadlessPermissionMode overrides the permission posture for this run.
	// "auto" emits --permission-mode auto (Claude Code auto-mode classifier).
	// "bypass" (or empty) keeps --dangerously-skip-permissions.
	// Only effective for claude headless runs when AllowedTools is empty and
	// RequirePermissions is false.
	HeadlessPermissionMode string
	// SandboxMode overrides the OS-level process-sandbox posture ("off",
	// "report", or "enforce") for this run. Set by the dispatcher
	// (agentorch.ResolveSandboxMode) from the task's Sandbox toggle merged
	// with config.DefaultSandboxMode(). Empty is treated as "report" by
	// Manager.injectProcessSandbox.
	SandboxMode string
	// PlaywrightMCPEligible marks a run as a candidate for the headless
	// Playwright MCP server (set by the workflow dispatcher for RoleTestRunner
	// runs only; see agentAdapter.StartAgent). Actually attaching MCP
	// additionally requires config.PlaywrightMCPEnabled, a headless run, and a
	// final-resolved Claude provider — decided by
	// Manager.preparePlaywrightMCP, never by the raw requested provider.
	PlaywrightMCPEligible bool
	// PlaywrightMCPOutputDir is the per-task directory the Playwright MCP
	// server writes screenshots/console logs to. Set by the workflow
	// dispatcher alongside PlaywrightMCPEligible; empty falls back to
	// <Dir>/.sybra-evidence.
	PlaywrightMCPOutputDir string
	// MCPConfigJSON is the Claude --mcp-config JSON payload for the headless
	// Playwright MCP server. Set only by Manager.preparePlaywrightMCP after
	// the final provider resolves to claude and the launcher preflight
	// succeeds. Claude-only: every other provider ignores this field.
	MCPConfigJSON string
	// provider is the implementation selected once at run start after health
	// gates and failover. Replay paths that do not have RunConfig resolve from
	// the persisted provider string instead.
	provider Provider
	// resolvedSandboxHome is the per-task sandbox home directory resolved by
	// injectSandboxHome, reused by injectProcessSandbox as one of the
	// sandbox's allowed write roots. Never set by callers.
	resolvedSandboxHome string
	// sandbox is the resolved OS-level process-sandbox spec for this run,
	// computed once by injectProcessSandbox and consumed by wrapInvocation
	// at each provider spawn site. Never set by callers.
	sandbox sandboxSpec
	// approvalAddr is the manager's HTTP approval-server address
	// ("127.0.0.1:port"), set by prepareRunConfig for every run. Consumed by
	// claudeProvider.BuildHeadlessInvocation to wire the PreToolUse approval
	// hook when RequirePermissions is true — without it, a headless run under
	// require_permissions:true has no path to grant a tool call and
	// operators are forced to require_permissions:false, which collapses to
	// --dangerously-skip-permissions. Never set by callers.
	approvalAddr string
}

// needsApprovalHook reports whether a run should wire the PreToolUse approval
// hook. True when permissions are required or an interactive permission-mode is
// set. Both the headless (provider_claude.go) and conversational
// (runner_convo.go) call sites gate on this so a future change can't silently
// desync them: headless runs never set PermissionMode (they use
// HeadlessPermissionMode for the auto classifier), so for them it collapses to
// RequirePermissions alone.
func (cfg RunConfig) needsApprovalHook() bool {
	return cfg.RequirePermissions || cfg.PermissionMode != ""
}

// ConvoOutput returns a snapshot of the conversation event buffer.
func (a *Agent) ConvoOutput() []ConvoEvent {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snapshot := make([]ConvoEvent, len(a.convoBuffer))
	copy(snapshot, a.convoBuffer)
	return snapshot
}

// SetError records a classified error on the agent.
func (a *Agent) SetError(kind, msg string) {
	a.mu.Lock()
	a.ErrorKind = kind
	a.ErrorMsg = msg
	a.mu.Unlock()
}

// GetErrorKind returns the classified error kind recorded on the agent
// ("rate_limit", "auth", or ""). The runner sets it when a run is classified
// against the provider health gate, letting the completion handler tell a
// transient provider limit apart from a real crash.
func (a *Agent) GetErrorKind() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ErrorKind
}
