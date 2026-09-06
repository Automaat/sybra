package agentqueue

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
)

func TestQueueTelemetryRestoredIdentityReplacesReofferedItem(t *testing.T) {
	type observed struct {
		enqueued time.Time
		state    string
	}
	var got []observed
	q, err := New(t.TempDir(), Options{Observe: func(_ string, at, _ time.Time, state string) { got = append(got, observed{at, state}) }}, nil)
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

func TestQueueTelemetryDoesNotHoldQueueLock(t *testing.T) {
	entered := make(chan time.Time, 1)
	release := make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	var events []audit.Event
	q, err := New(t.TempDir(), Options{Observe: func(id string, enqueued, at time.Time, state string) {
		if state == "queued" {
			entered <- at
			<-release
		}
		mu.Lock()
		defer mu.Unlock()
		events = append(events, audit.Event{Type: audit.EventFactoryQueue, Timestamp: at, Data: map[string]any{
			"interval_key": audit.FactoryIntervalKey(id, enqueued.Format(time.RFC3339Nano)), "state": state,
		}})
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	q.now = func() time.Time { return base }
	offered := make(chan struct{})
	go func() { q.Offer(Item{TaskID: "synthetic"}); close(offered) }()
	select {
	case at := <-entered:
		if !at.Equal(base) {
			t.Fatalf("boundary timestamp = %v, want %v", at, base)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offer did not reach observer")
	}
	popped := make(chan []Item, 1)
	go func() { popped <- q.PopReady(1) }()
	select {
	case items := <-popped:
		if len(items) != 1 || items[0].TaskID != "synthetic" {
			t.Fatalf("concurrent pop = %+v", items)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow telemetry blocked queue operations")
	}
	// Release before waiting, while retaining the deferred cleanup on failures.
	release <- struct{}{}
	<-offered
	if len(events) != 2 || events[0].Data["state"] != "dequeued" {
		t.Fatalf("probe did not reverse delivery: %+v", events)
	}
	report, err := audit.SummarizeFactory(events, audit.FactoryQuery{Since: base.Add(-time.Second), Until: base.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if phase := report.Phases["queue"]; phase.Samples != 1 || phase.Open != 0 || phase.Unknown != 0 {
		t.Fatalf("coarse clock plus reversed delivery lost interval: %+v", phase)
	}
}
