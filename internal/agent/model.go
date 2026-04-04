package agent

import (
	"context"
	"os/exec"
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
	External     bool      `json:"external"`
	PID          int       `json:"pid,omitempty"`
	Command      string    `json:"command,omitempty"`
	Name         string    `json:"name,omitempty"`
	Project      string    `json:"project,omitempty"`
	Model        string    `json:"model,omitempty"`

	ExitErr      error `json:"-"`
	outputBuffer []StreamEvent
	cmd          *exec.Cmd
	cancel       context.CancelFunc
	sessionCWD   string
	// done is closed when the headless goroutine has fully exited.
	// Used by HasRunningAgentForTask to guard worktree cleanup.
	done chan struct{}
}

func (a *Agent) Output() []StreamEvent {
	return a.outputBuffer
}

// RunConfig is the single entry point for starting any agent.
type RunConfig struct {
	TaskID       string
	Name         string
	Mode         string // "headless" or "interactive"
	Prompt       string
	AllowedTools []string
	Dir          string
	Model        string // "opus", "sonnet", or full model ID
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
