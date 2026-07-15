// Package bgop tracks long-running background operations (clone, worktree prep)
// and emits Wails events so the frontend can show progress indicators.
package bgop

import "time"

// Type classifies a background operation.
type Type string

const (
	// TypeClone is a bare-repo git clone.
	TypeClone Type = "clone"
	// TypeWorktreePrep is worktree creation/rebase/push for an agent.
	TypeWorktreePrep Type = "worktree_prep"
)

// Status is the lifecycle state of an operation.
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Operation is a single tracked background operation.
type Operation struct {
	ID          string    `json:"id"`
	Type        Type      `json:"type"`
	Label       string    `json:"label"`
	Status      Status    `json:"status"`
	Phase       string    `json:"phase,omitempty"`
	ProjectID   string    `json:"projectId,omitempty"`
	TaskID      string    `json:"taskId,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt time.Time `json:"completedAt,omitzero"`
	Error       string    `json:"error,omitempty"`
}
