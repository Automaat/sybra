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
	ID          string    `json:"id"`
	TaskID      string    `json:"taskId"`
	Mode        string    `json:"mode"`
	State       State     `json:"state"`
	SessionID   string    `json:"sessionId"`
	TmuxSession string    `json:"tmuxSession"`
	CostUSD     float64   `json:"costUsd"`
	StartedAt   time.Time `json:"startedAt"`

	outputBuffer []StreamEvent
	cmd          *exec.Cmd
	cancel       context.CancelFunc
}

func (a *Agent) Output() []StreamEvent {
	return a.outputBuffer
}

type StreamEvent struct {
	Type      string  `json:"type"`
	Content   string  `json:"content,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}
