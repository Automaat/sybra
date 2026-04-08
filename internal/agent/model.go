package agent

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"time"
)

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
}

func (a *Agent) Output() []StreamEvent {
	return a.outputBuffer
}

// RunConfig is the single entry point for starting any agent.
type RunConfig struct {
	TaskID             string
	Name               string
	Mode               string // "headless", "interactive", or "conversational"
	Prompt             string
	AllowedTools       []string
	Dir                string
	Model              string // "opus", "sonnet", or full model ID
	RequirePermissions bool   // when true, suppress --dangerously-skip-permissions
	PermissionMode     string // "default", "acceptEdits", "bypassPermissions" (conversational mode)
	Effort             string // "low", "medium", "high", "max" (extended thinking)
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

// ConvoOutput returns the conversation event buffer.
func (a *Agent) ConvoOutput() []ConvoEvent {
	return a.convoBuffer
}

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
