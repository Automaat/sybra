package sybra

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/task"
)

func TestQueueService_SnapshotDepth(t *testing.T) {
	queue, err := agentqueue.New(t.TempDir(), agentqueue.Options{MaxDepth: 2}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if added := queue.Offer(agentqueue.Item{
		TaskID:   "promoted",
		Priority: task.PriorityNone,
		Status:   task.StatusInReview,
		Enqueued: now,
	}); !added {
		t.Fatal("Offer(promoted) returned false, want true")
	}
	if added := queue.Offer(agentqueue.Item{
		TaskID:   "medium",
		Priority: task.PriorityMedium,
		Enqueued: now.Add(time.Minute),
	}); !added {
		t.Fatal("Offer(medium) returned false, want true")
	}

	svc := &QueueService{queue: queue}
	if got := svc.SnapshotDepth(); got != (QueueDepthSnapshot{
		Depth:                2,
		TopEffectivePriority: string(task.PriorityHigh),
	}) {
		t.Fatalf("SnapshotDepth() = %+v, want depth=2 top=%q", got, task.PriorityHigh)
	}

	var nilSvc QueueService
	if got := nilSvc.SnapshotDepth(); got != (QueueDepthSnapshot{}) {
		t.Fatalf("nil SnapshotDepth() = %+v, want zero value", got)
	}
}
