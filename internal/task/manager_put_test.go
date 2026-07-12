package task

import (
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/events"
)

type hookCall struct {
	id, from, to string
}

func TestManagerPutFiresStatusHookForStageStatus(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)
	var mu sync.Mutex
	var calls []hookCall
	m.SetStatusChangeHook(func(id, from, to string) {
		mu.Lock()
		calls = append(calls, hookCall{id, from, to})
		mu.Unlock()
	})

	if _, err := m.Put(Task{ID: "leader-1", Title: "t", Status: StatusReadyReview, AgentMode: AgentModeHeadless}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("status hook fired %d times, want 1 (stage dispatch would never run otherwise)", len(calls))
	}
	if calls[0].to != string(StatusReadyReview) || calls[0].from != "" {
		t.Fatalf("hook call = %+v, want from='' to=ready-review", calls[0])
	}
	if names := emitter.names(); len(names) != 1 || names[0] != events.TaskCreated {
		t.Fatalf("events = %v, want [task:created]", names)
	}
}

func TestManagerPutFiresHookOnTransition(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	var mu sync.Mutex
	var calls []hookCall
	m.SetStatusChangeHook(func(id, from, to string) {
		mu.Lock()
		calls = append(calls, hookCall{id, from, to})
		mu.Unlock()
	})

	base := Task{ID: "leader-2", Title: "t", Status: StatusInProgress, AgentMode: AgentModeHeadless}
	if _, err := m.Put(base); err != nil {
		t.Fatal(err)
	}
	base.Status = StatusTesting
	if _, err := m.Put(base); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("hook fired %d times, want 2 (initial + transition)", len(calls))
	}
	if calls[1].from != string(StatusInProgress) || calls[1].to != string(StatusTesting) {
		t.Fatalf("transition hook = %+v, want in-progress->testing", calls[1])
	}
}

func TestManagerPutNoHookWhenStatusUnchanged(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)
	var mu sync.Mutex
	fired := 0
	m.SetStatusChangeHook(func(string, string, string) {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	base := Task{ID: "leader-3", Title: "t", Status: StatusTodo, AgentMode: AgentModeHeadless}
	if _, err := m.Put(base); err != nil {
		t.Fatal(err)
	}
	base.Title = "renamed"
	if _, err := m.Put(base); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Fatalf("hook fired %d times, want 1 (only the initial create; a title-only re-push must not re-fire)", fired)
	}
}
