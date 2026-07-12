package sybra

import "github.com/Automaat/sybra/internal/agentqueue"

// QueueDepthSnapshot is the HTTP-facing queue depth snapshot payload.
type QueueDepthSnapshot struct {
	Depth                int    `json:"depth"`
	TopEffectivePriority string `json:"topEffectivePriority"`
}

// QueueService exposes queue readouts over the HTTP control plane only.
type QueueService struct {
	queue *agentqueue.Queue
}

// SnapshotDepth returns the current queue depth and top effective priority.
func (s *QueueService) SnapshotDepth() QueueDepthSnapshot {
	if s.queue == nil {
		return QueueDepthSnapshot{}
	}
	snap := s.queue.DepthSnapshot()
	return QueueDepthSnapshot{
		Depth:                snap.Depth,
		TopEffectivePriority: string(snap.TopEffectivePriority),
	}
}
