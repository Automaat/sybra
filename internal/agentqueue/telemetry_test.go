package agentqueue

import (
	"slices"
	"testing"
	"time"
)

func TestQueueTelemetryRestoredIdentityReplacesReofferedItem(t *testing.T) {
	type observed struct {
		enqueued time.Time
		state    string
	}
	var got []observed
	q, err := New(t.TempDir(), Options{Observe: func(_ string, at time.Time, state string) { got = append(got, observed{at, state}) }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	old := Item{TaskID: "synthetic", Enqueued: base}
	q.Offer(old)
	q.PopReady(1)
	q.Offer(Item{TaskID: "synthetic", Enqueued: base.Add(time.Minute)})
	q.Restore(old)
	q.PopReady(1)
	want := []observed{{base, "queued"}, {base, "dequeued"}, {base.Add(time.Minute), "queued"}, {base.Add(time.Minute), "removed"}, {base, "queued"}, {base, "dequeued"}}
	if !slices.Equal(got, want) {
		t.Fatalf("boundaries: %+v, want %+v", got, want)
	}
}
