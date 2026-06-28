package agent

import (
	"context"
	"io"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// NOTE on concurrency: Agent has two distinct mutexes.
//   - mu: guards all mutable scalar and slice fields (State, outputBuffer,
//     convoBuffer, LastEventAt, CostUSD, TurnCount, SessionID, ExitErr,
//     cmd, PID, LogPath, EscalationReason). Use the helper methods
//     defined on Agent rather than touching the fields directly from
//     concurrent code paths.
//   - stdinMu: guards stdinPipe only. Kept separate because the runner
//     goroutine may hold it for the duration of a blocking Write, and
//     we do not want to starve other consumers that only need to read
//     State or append an event.

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
	// lastToolSig / toolLoopStreak track consecutive identical tool-call
	// signatures so the watchdog can detect an agent looping on the same call
	// in real time (it never stalls, so the stall trigger misses it). Guarded
	// by mu; updated from the headless stream via NoteToolSignature.
	lastToolSig    string
	toolLoopStreak int
	// loopAckSig is the signature the watchdog has already inspected and
	// decided not to kill. While it equals lastToolSig the loop trigger is
	// suppressed, so a cleared (or legitimately repetitive) loop is not
	// re-inspected every debounce window — and a now-frozen high streak no
	// longer masks the stall trigger. Cleared implicitly when the signature
	// changes (a genuinely new loop re-arms the trigger).
	loopAckSig string
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
	if sig == "" {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.toolLoopStreak
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if sig == a.lastToolSig {
		a.toolLoopStreak++
	} else {
		a.lastToolSig = sig
		a.toolLoopStreak = 1
	}
	return a.toolLoopStreak
}

// ToolLoopStreak returns the current count of consecutive identical tool-call
// signatures. A high value means the agent is repeating the same call.
func (a *Agent) ToolLoopStreak() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.toolLoopStreak
}

// AckToolLoop records the current loop signature as already inspected, so the
// watchdog does not re-trigger on the same unchanged loop. Called after a
// loop-triggered inspection whose verdict left the agent running.
func (a *Agent) AckToolLoop() {
	a.mu.Lock()
	a.loopAckSig = a.lastToolSig
	a.mu.Unlock()
}

// ToolLoopAcknowledged reports whether the current loop signature has already
// been inspected (and the agent left running). True suppresses the loop
// trigger until the signature changes.
func (a *Agent) ToolLoopAcknowledged() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.loopAckSig != "" && a.loopAckSig == a.lastToolSig
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
