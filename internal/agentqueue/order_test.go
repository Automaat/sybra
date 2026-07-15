package agentqueue

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

func TestEffectivePriority(t *testing.T) {
	tests := []struct {
		name string
		item Item
		want task.Priority
	}{
		{
			name: "declared priority wins when above floor",
			item: Item{Priority: task.PriorityUrgent, Status: task.StatusTodo},
			want: task.PriorityUrgent,
		},
		{
			name: "review status floors to high",
			item: Item{Priority: task.PriorityLow, Status: task.StatusInReview},
			want: task.PriorityHigh,
		},
		{
			name: "testing status floors to medium",
			item: Item{Priority: task.PriorityNone, Status: task.StatusTesting},
			want: task.PriorityMedium,
		},
		{
			name: "normal status keeps none",
			item: Item{Priority: task.PriorityNone, Status: task.StatusTodo},
			want: task.PriorityNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.item.EffectivePriority(); got != tt.want {
				t.Fatalf("EffectivePriority() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLessUsesStaticPriorityNotStarvationBoost(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	old := Item{
		TaskID:   "old",
		Priority: task.PriorityNone,
		Enqueued: now.Add(-2 * time.Hour),
	}
	fresh := Item{
		TaskID:   "fresh",
		Priority: task.PriorityLow,
		Enqueued: now,
	}

	if Less(old, fresh) {
		t.Fatal("Less(old, fresh) = true, want static snapshot ordering to keep fresh ahead")
	}
	if !lessBoosted(old, fresh, now, time.Hour) {
		t.Fatal("lessBoosted(old, fresh) = false, want starvation boost to affect dispatch only")
	}
}
