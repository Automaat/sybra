package sybra

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
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

func TestQueueService_AgentQueueSnapshot(t *testing.T) {
	queue, err := agentqueue.New(t.TempDir(), agentqueue.Options{StarvationBoostAfter: time.Hour}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if added := queue.Offer(agentqueue.Item{
		TaskID:   "fresh-low",
		Role:     string(agent.RoleImplementation),
		Priority: task.PriorityLow,
		Status:   task.StatusTodo,
		Manual:   true,
		Mode:     "headless",
		Prompt:   "do not leak",
		Enqueued: now,
	}); !added {
		t.Fatal("Offer(fresh-low) returned false, want true")
	}
	if added := queue.Offer(agentqueue.Item{
		TaskID:   "old-none",
		Role:     string(agent.RoleImplementation),
		Priority: task.PriorityNone,
		Status:   task.StatusInReview,
		Manual:   false,
		Mode:     "interactive",
		Prompt:   "still do not leak",
		Enqueued: now.Add(-2 * time.Hour),
	}); !added {
		t.Fatal("Offer(old-none) returned false, want true")
	}

	svc := &QueueService{queue: queue}
	got := svc.AgentQueueSnapshot()
	if got.Depth != 2 {
		t.Fatalf("Depth = %d, want 2", got.Depth)
	}
	if len(got.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(got.Items))
	}
	if got.Items[0].TaskID != "old-none" {
		t.Fatalf("Items[0].TaskID = %q, want old-none (status floor/static snapshot ordering)", got.Items[0].TaskID)
	}
	if got.Items[0].Position != 1 || got.Items[0].Depth != 2 {
		t.Fatalf("Items[0] queue numbers = position %d depth %d, want 1 and 2", got.Items[0].Position, got.Items[0].Depth)
	}
	if got.Items[0].EffectivePriority != string(task.PriorityHigh) {
		t.Fatalf("Items[0].EffectivePriority = %q, want %q", got.Items[0].EffectivePriority, task.PriorityHigh)
	}
	if got.Items[1].TaskID != "fresh-low" {
		t.Fatalf("Items[1].TaskID = %q, want fresh-low", got.Items[1].TaskID)
	}
	if got.Items[1].Position != 2 || got.Items[1].Depth != 2 {
		t.Fatalf("Items[1] queue numbers = position %d depth %d, want 2 and 2", got.Items[1].Position, got.Items[1].Depth)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot): %v", err)
	}
	if jsonContainsField(encoded, "prompt") {
		t.Fatalf("snapshot JSON leaked prompt field: %s", encoded)
	}

	var nilSvc QueueService
	nilSnap := nilSvc.AgentQueueSnapshot()
	if nilSnap.Depth != 0 || len(nilSnap.Items) != 0 {
		t.Fatalf("nil AgentQueueSnapshot() = %+v, want empty snapshot", nilSnap)
	}
}

func jsonContainsField(data []byte, field string) bool {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return mapContainsField(raw, field)
}

func mapContainsField(v any, field string) bool {
	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			if k == field || mapContainsField(child, field) {
				return true
			}
		}
	case []any:
		for i := range typed {
			if mapContainsField(typed[i], field) {
				return true
			}
		}
	}
	return false
}
