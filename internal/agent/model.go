package agent

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
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

type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
)

type Agent struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"taskId"`
	Mode         string    `json:"mode"`
	State        State     `json:"state"`
	SessionID    string    `json:"sessionId"`
	CostUSD      float64   `json:"costUsd"`
	InputTokens  int       `json:"inputTokens,omitempty"`
	OutputTokens int       `json:"outputTokens,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	LastEventAt  time.Time `json:"lastEventAt"`
	LogPath      string    `json:"logPath,omitempty"`
	External     bool      `json:"external"`
	PID          int       `json:"pid,omitempty"`
	Command      string    `json:"command,omitempty"`
	Name         string    `json:"name,omitempty"`
	Project      string    `json:"project,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`

	TurnCount        int    `json:"turnCount,omitempty"`
	EscalationReason string `json:"escalationReason,omitempty"`
	ErrorKind        string `json:"errorKind,omitempty"`
	ErrorMsg         string `json:"errorMsg,omitempty"`
	AwaitingApproval bool   `json:"awaitingApproval,omitempty"`

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

	// escalationCh receives the human's decision when a guardrail is hit.
	// true = continue, false = kill.
	escalationCh chan bool

	// Conversational mode fields
	stdinPipe  io.WriteCloser
	stdinMu    sync.Mutex
	approvalCh chan ApprovalResponse

	// pendingPrompts queues follow-up user messages that arrive while a turn
	// is mid-flight. Drained after each "result" event so the next turn fires
	// without waiting on the user. Guarded by mu.
	pendingPrompts []string

	// promptCh delivers follow-up prompts to Codex conversational agents.
	// Each turn spawns a new codex exec process; promptCh signals the next
	// prompt without a stdin pipe. Guarded by mu.
	promptCh chan string

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
func (a *Agent) AddResultStats(sessionID string, cost float64, in, out int) float64 {
	a.mu.Lock()
	if sessionID != "" {
		a.SessionID = sessionID
	}
	a.CostUSD += cost
	a.InputTokens += in
	a.OutputTokens += out
	result := a.CostUSD
	a.mu.Unlock()
	return result
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

// SetEscalationReason updates the escalation reason string.
func (a *Agent) SetEscalationReason(reason string) {
	a.mu.Lock()
	a.EscalationReason = reason
	a.mu.Unlock()
}

// GetCostUSD returns the current cumulative cost.
func (a *Agent) GetCostUSD() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CostUSD
}

// GetLogPath returns the current output log path.
func (a *Agent) GetLogPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.LogPath
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
	Provider           string // "claude" or "codex"
	Model              string // "opus", "sonnet", or full model ID
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
	// ResumeSessionID, when set, passes --resume to the claude CLI so the
	// agent continues a prior conversation instead of starting from scratch.
	// Populated from the task's last AgentRun.SessionID on restart.
	ResumeSessionID string
	// ExtraEnv is a list of "KEY=VALUE" strings appended to the subprocess
	// environment. Used to inject sandbox credentials (SANDBOX_URL, KUBECONFIG).
	ExtraEnv []string
}

// PlanStep represents a single item from a TodoWrite tool call.
type PlanStep struct {
	Content string `json:"content"`
	Status  string `json:"status"` // "pending", "in_progress", "completed"
}

type StreamEvent struct {
	Type         string    `json:"type"`
	Content      string    `json:"content,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	CostUSD      float64   `json:"cost_usd,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	Subtype      string    `json:"subtype,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	// ErrorType and ErrorStatus carry structured fields from the Anthropic error
	// envelope (e.g. "overloaded_error", 529) when subtype == "error".
	ErrorType   string `json:"error_type,omitempty"`
	ErrorStatus int    `json:"error_status,omitempty"`
	// PlanSteps is populated when the assistant calls TodoWrite; contains the
	// latest snapshot of the agent's todo list at this point in the stream.
	PlanSteps []PlanStep `json:"plan_steps,omitempty"`
}

// ConvoEvent is a rich event for conversational mode, preserving full tool
// call structure for the chat UI.
type ConvoEvent struct {
	Type         string            `json:"type"`
	Subtype      string            `json:"subtype,omitempty"`
	SessionID    string            `json:"sessionId,omitempty"`
	Text         string            `json:"text,omitempty"`
	ToolUses     []ToolUseBlock    `json:"toolUses,omitempty"`
	ToolResults  []ToolResultBlock `json:"toolResults,omitempty"`
	CostUSD      float64           `json:"costUsd,omitempty"`
	InputTokens  int               `json:"inputTokens,omitempty"`
	OutputTokens int               `json:"outputTokens,omitempty"`
	IsPartial    bool              `json:"isPartial,omitempty"`
	Timestamp    time.Time         `json:"timestamp"`
	Raw          json.RawMessage   `json:"raw,omitempty"`
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

// ErrorEvent is the payload emitted on agent:error:{id}.
type ErrorEvent struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
