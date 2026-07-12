package sybra

import (
	"time"

	"github.com/Automaat/sybra/internal/agentqueue"
)

// QueueDepthSnapshot is the HTTP-facing queue depth snapshot payload.
type QueueDepthSnapshot struct {
	Depth                int    `json:"depth"`
	TopEffectivePriority string `json:"topEffectivePriority"`
}

// AgentQueueSnapshot is the read-only queued-agent view exposed to the GUI.
type AgentQueueSnapshot struct {
	Depth int                      `json:"depth"`
	Items []AgentQueueSnapshotItem `json:"items"`
}

// AgentQueueSnapshotItem is one queue row, ordered by Queue.Snapshot().
type AgentQueueSnapshotItem struct {
	TaskID            string    `json:"taskId"`
	Role              string    `json:"role"`
	Position          int       `json:"position"`
	Depth             int       `json:"depth"`
	Priority          string    `json:"priority"`
	EffectivePriority string    `json:"effectivePriority"`
	Status            string    `json:"status"`
	Manual            bool      `json:"manual"`
	Mode              string    `json:"mode"`
	Enqueued          time.Time `json:"enqueued"`
}

// QueueService exposes queue readouts over the HTTP control plane only.
type QueueService struct {
	queue *agentqueue.Queue
}

// SnapshotDepth returns the current queue depth and top effective priority.
func (s *QueueService) SnapshotDepth() QueueDepthSnapshot {
	if s == nil || s.queue == nil {
		return QueueDepthSnapshot{}
	}
	snap := s.queue.DepthSnapshot()
	return QueueDepthSnapshot{
		Depth:                snap.Depth,
		TopEffectivePriority: string(snap.TopEffectivePriority),
	}
}

// AgentQueueSnapshot returns the queue's ordered items plus top-level depth.
func (s *QueueService) AgentQueueSnapshot() AgentQueueSnapshot {
	if s == nil || s.queue == nil {
		return AgentQueueSnapshot{Items: []AgentQueueSnapshotItem{}}
	}
	snap := s.queue.Snapshot()
	out := AgentQueueSnapshot{
		Depth: len(snap),
		Items: make([]AgentQueueSnapshotItem, 0, len(snap)),
	}
	for i := range snap {
		it := snap[i]
		out.Items = append(out.Items, AgentQueueSnapshotItem{
			TaskID:            it.TaskID,
			Role:              it.Role,
			Position:          i + 1,
			Depth:             len(snap),
			Priority:          string(it.Priority),
			EffectivePriority: string(it.EffectivePriority()),
			Status:            string(it.Status),
			Manual:            it.Manual,
			Mode:              it.Mode,
			Enqueued:          it.Enqueued,
		})
	}
	return out
}
