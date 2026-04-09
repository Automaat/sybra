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
	TmuxSession  string    `json:"tmuxSession"`
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

	ExitErr      error `json:"-"`
	outputBuffer []StreamEvent
	convoBuffer  []ConvoEvent
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	sessionCWD   string
	// done is closed when the headless/conversational goroutine has fully exited.
	// Used by HasRunningAgentForTask to guard worktree cleanup.
	done chan struct{}

	// escalationCh receives the human's decision when a guardrail is hit.
	// true = continue, false = kill.
	escalationCh chan bool

	// Conversational mode fields
	stdinPipe  io.WriteCloser
	stdinMu    sync.Mutex
	approvalCh chan ApprovalResponse

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

// SetSessionID records the Claude session ID.
func (a *Agent) SetSessionID(id string) {
	a.mu.Lock()
	a.SessionID = id
	a.mu.Unlock()
}

// GetSessionID returns the current Claude session ID.
func (a *Agent) GetSessionID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SessionID
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
}

type StreamEvent struct {
	Type         string  `json:"type"`
	Content      string  `json:"content,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	Subtype      string  `json:"subtype,omitempty"`
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

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
