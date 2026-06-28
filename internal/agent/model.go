package agent

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

// NOTE on concurrency: Agent has distinct mutexes.
//   - mu: guards all mutable scalar and slice fields (State, outputBuffer,
//     convoBuffer, LastEventAt, CostUSD, TurnCount, SessionID, ExitErr,
//     cmd, PID, LogPath, EscalationReason). Use the helper methods
//     defined on Agent rather than touching the fields directly from
//     concurrent code paths.
//   - stdinMu: guards stdinPipe only. Kept separate because the runner
//     goroutine may hold it for the duration of a blocking Write, and
//     we do not want to starve other consumers that only need to read
//     State or append an event.
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

	// escalationCh receives the human's decision when a guardrail is hit.
	// true = continue, false = kill.
	escalationCh chan bool

	// Conversational mode fields
	stdinPipe  io.WriteCloser
	stdinMu    sync.Mutex
	approvalCh chan ApprovalResponse

	// stdinPath is the FIFO backing a detached conversational agent's stdin,
	// reopened on reattach so follow-up messages survive a restart. Empty for
	// pipe-backed (non-survival) agents. Guarded by mu.
	stdinPath string

	// pendingPrompts queues follow-up user messages that arrive while a turn
	// is mid-flight. Drained after each "result" event so the next turn fires
	// without waiting on the user. Guarded by mu.
	pendingPrompts []string

	// promptCh delivers follow-up prompts to Codex conversational agents.
	// Each turn spawns a new codex exec process; promptCh signals the next
	// prompt without a stdin pipe. Guarded by mu.
	promptCh chan string

	// stopped is set by StopAgent before cancelling the context so
	// OnComplete can distinguish an intentional user stop (SIGTERM via
	// cancel) from an infra-level kill (OS/container SIGTERM). Both exit
	// with a signal, but only intentional stops should advance the
	// workflow step to "failed".
	stopped bool

	// detached is true when the agent's subprocess was spawned to survive
	// an app restart (Setsid, output redirected to its log file, no ctx
	// kill). ShutdownWithGrace leaves detached agents running instead of
	// cancelling them. Guarded by mu.
	detached bool

	// requirePermissions mirrors RunConfig.RequirePermissions. Persisted to
	// the registry so a recreated codex chat keeps its sandbox/approval
	// choice across a restart instead of silently becoming permissive.
	requirePermissions bool

	// headlessPermissionMode is the resolved posture passed via RunConfig
	// ("bypass" or "auto"). Stored for OnComplete so the denial audit events
	// can record the posture without re-resolving it.
	headlessPermissionMode string

	// permissionDenials accumulates auto-mode classifier denial records
	// observed during the run. Flushed to audit in OnComplete.
	permissionDenials []PermissionDenial

	// mu guards mutable fields touched from multiple goroutines. See the
	// package-level note above the Agent type.
	mu sync.RWMutex
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
		StdinPath:          a.stdinPath,
		OneShot:            a.oneShot,
		MaxTurns:           a.MaxTurns,
		RequirePermissions: a.requirePermissions,
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
		stdinPath:          r.StdinPath,
		requirePermissions: r.RequirePermissions,
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

// AddResultStats merges a result-event's stats into the running totals
// and returns the new cumulative CostUSD.
func (a *Agent) AddResultStats(sessionID string, cost float64, in, out, reasoning int) float64 {
	a.mu.Lock()
	if sessionID != "" {
		a.SessionID = sessionID
	}
	a.CostUSD += cost
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
	a.pendingPrompts = append(a.pendingPrompts, text)
	a.mu.Unlock()
}

// PopPendingPrompt returns the next queued prompt and a flag indicating
// whether a value was popped.
func (a *Agent) PopPendingPrompt() (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingPrompts) == 0 {
		return "", false
	}
	next := a.pendingPrompts[0]
	a.pendingPrompts = a.pendingPrompts[1:]
	return next, true
}

// PendingPromptCount returns the size of the pending prompt queue.
func (a *Agent) PendingPromptCount() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.pendingPrompts)
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
	a.stdinPath = p
	a.mu.Unlock()
}

// GetStdinPath returns the FIFO path backing the agent's stdin ("" for
// pipe-backed agents).
func (a *Agent) GetStdinPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.stdinPath
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
	Effort             string // "low", "medium", "high", "max" (extended thinking)
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
	// environment. Used to inject sandbox credentials (SANDBOX_URL, KUBECONFIG).
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
	// ReasoningEffort sets codex's model_reasoning_effort (low/medium/high/xhigh)
	// for this run. Empty = model default. Codex-only. NOT the same as Effort
	// (claude --effort) — different provider, CLI surface, and value set.
	ReasoningEffort string
	// SeedWorkingMemory, when true, inlines the worktree's NOTES.md scratchpad
	// into the prompt (read/maintain instruction + current contents). Set only
	// for code-author roles (see Role.AuthorsCode): verifier roles share the
	// implementation worktree, so seeding them would feed an independent
	// reviewer/tester the implementer's notes. No-op if the dir has no NOTES.md.
	SeedWorkingMemory bool
	// OutputSchema is an inline JSON Schema (codex only). The runner writes it
	// to a temp file and passes --output-schema <path> to codex exec. Empty =
	// no schema enforcement. Ignored by claude/copilot.
	OutputSchema string
	// outputSchemaPath is the temp file path the runner wrote OutputSchema to.
	// Set intra-package before buildHeadlessInvocation; cleared by defer after
	// the subprocess exits. Never set by callers.
	outputSchemaPath string
	// HeadlessPermissionMode overrides the permission posture for this run.
	// "auto" emits --permission-mode auto (Claude Code auto-mode classifier).
	// "bypass" (or empty) keeps --dangerously-skip-permissions.
	// Only effective for claude headless runs when AllowedTools is empty and
	// RequirePermissions is false.
	HeadlessPermissionMode string
	// provider is the implementation selected once at run start after health
	// gates and failover. Replay paths that do not have RunConfig resolve from
	// the persisted provider string instead.
	provider Provider
}

// PermissionDenial records a single auto-mode classifier denial observed during
// a headless run. Populated from tool_result error blocks that match the Claude
// Code auto-mode classifier denial marker.
type PermissionDenial struct {
	ToolUseID string
	Reason    string
}

// PlanStep represents a single item from a TodoWrite tool call.
type PlanStep struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending", "in_progress", "completed"
}

type StreamEvent struct {
	Type                     string  `json:"type"`
	Content                  string  `json:"content,omitempty"`
	SessionID                string  `json:"session_id,omitempty"`
	CostUSD                  float64 `json:"cost_usd,omitempty"`
	InputTokens              int     `json:"input_tokens,omitempty"`
	OutputTokens             int     `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens,omitempty"`
	ReasoningTokens          int     `json:"reasoning_tokens,omitempty"`
	// PremiumRequests is Copilot's per-result billing-unit count (result event)
	// or 0 for claude/codex.
	PremiumRequests float64   `json:"premium_requests,omitempty"`
	Subtype         string    `json:"subtype,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
	// ErrorType and ErrorStatus carry structured fields from the Anthropic error
	// envelope (e.g. "overloaded_error", 529) when subtype == "error".
	ErrorType   string `json:"error_type,omitempty"`
	ErrorStatus int    `json:"error_status,omitempty"`
	// PlanSteps is populated when the assistant calls TodoWrite; contains the
	// latest snapshot of the agent's todo list at this point in the stream.
	PlanSteps []PlanStep `json:"plan_steps,omitempty"`
	// PluginErrors carries plugin load failures surfaced by the init event.
	PluginErrors []string `json:"plugin_errors,omitempty"`
	// ToolCalls is the number of tool_use blocks in this event: all tool uses
	// in a Claude assistant turn, or a single Codex tool_use. The runner
	// accumulates these into Agent.ToolCalls.
	ToolCalls int `json:"tool_calls,omitempty"`
	// LimitSnapshot carries provider quota status emitted by CLIs such as
	// Codex. It is forwarded to the limits ledger and not rendered as normal
	// assistant output.
	LimitSnapshot *limits.Snapshot `json:"limit_snapshot,omitempty"`
	// toolSig is a canonical fingerprint of this event's tool calls (name +
	// input), used by the watchdog's real-time loop detector to spot an agent
	// repeating the same call. Unexported so it is never serialized to the
	// NDJSON log or emitted to the frontend; it lives only in memory.
	toolSig string
	// permissionDenials carries auto-mode classifier denial records extracted
	// from this event's tool_result error blocks. Unexported so it is never
	// serialized; lives in-memory only. Populated for claude "user" events only.
	permissionDenials []PermissionDenial
}

// ConvoEvent is a rich event for conversational mode, preserving full tool
// call structure for the chat UI.
type ConvoEvent struct {
	Type                     string            `json:"type"`
	Subtype                  string            `json:"subtype,omitempty"`
	SessionID                string            `json:"sessionId,omitempty"`
	Text                     string            `json:"text,omitempty"`
	ToolUses                 []ToolUseBlock    `json:"toolUses,omitempty"`
	ToolResults              []ToolResultBlock `json:"toolResults,omitempty"`
	CostUSD                  float64           `json:"costUsd,omitempty"`
	InputTokens              int               `json:"inputTokens,omitempty"`
	OutputTokens             int               `json:"outputTokens,omitempty"`
	CacheCreationInputTokens int               `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens     int               `json:"cacheReadInputTokens,omitempty"`
	ReasoningTokens          int               `json:"reasoningTokens,omitempty"`
	PremiumRequests          float64           `json:"premiumRequests,omitempty"`
	LimitSnapshot            *limits.Snapshot  `json:"limitSnapshot,omitempty"`
	IsPartial                bool              `json:"isPartial,omitempty"`
	Timestamp                time.Time         `json:"timestamp"`
	Raw                      json.RawMessage   `json:"raw,omitempty"`
	// ErrorType and ErrorStatus carry structured fields from the Anthropic error
	// envelope (e.g. "overloaded_error", 529) when subtype == "error".
	ErrorType   string `json:"errorType,omitempty"`
	ErrorStatus int    `json:"errorStatus,omitempty"`
}

// ToolUseBlock represents a single tool call from the assistant.
type ToolUseBlock struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResultBlock represents the result of a tool execution.
type ToolResultBlock struct {
	ToolUseID string `json:"toolUseId"`
	Content   string `json:"content"`
	IsError   bool   `json:"isError,omitempty"`
}

// ApprovalRequest is sent to the frontend when a tool needs user approval.
type ApprovalRequest struct {
	ToolUseID string         `json:"toolUseId"`
	ToolName  string         `json:"toolName"`
	Input     map[string]any `json:"input"`
}

// ApprovalResponse carries the user's decision from the frontend.
type ApprovalResponse struct {
	ToolUseID string `json:"toolUseId"`
	Approved  bool   `json:"approved"`
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

// ErrorEvent is the payload emitted on agent:error:{id}.
type ErrorEvent struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// PluginErrorsEvent is emitted on agent:plugin_errors:{id} when the init event
// carries plugin load failures.
type PluginErrorsEvent struct {
	Errors []string `json:"errors"`
}

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
