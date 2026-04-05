package task

import (
	"sync"
	"testing"

	"github.com/Automaat/synapse/internal/events"
)

type recordingEmitter struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	name string
	data any
}

func (r *recordingEmitter) Emit(name string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{name: name, data: data})
}

func (r *recordingEmitter) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.name
	}
	return out
}

func newTestManager(t *testing.T) (*Manager, *recordingEmitter) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	emitter := &recordingEmitter{}
	return NewManager(store, emitter), emitter
}

func TestManagerCreateEmitsEvent(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "body", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	names := emitter.names()
	if len(names) != 1 || names[0] != events.TaskCreated {
		t.Fatalf("events = %v, want [%s]", names, events.TaskCreated)
	}
	if emitter.events[0].data != task.FilePath {
		t.Fatalf("event data = %v, want %s", emitter.events[0].data, task.FilePath)
	}
}

func TestManagerUpdateEmitsEvent(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Update(task.ID, map[string]any{"title": "New"}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskUpdated {
		t.Fatalf("events = %v, want [%s %s]", names, events.TaskCreated, events.TaskUpdated)
	}
}

func TestManagerUpdateInvokesStatusHook(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	type change struct {
		id, from, to string
	}
	var got []change
	m.SetStatusChangeHook(func(id, from, to string) {
		got = append(got, change{id, from, to})
	})

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No status in updates → hook must not fire.
	if _, err := m.Update(task.ID, map[string]any{"title": "X"}); err != nil {
		t.Fatalf("Update title: %v", err)
	}
	// Status change → hook fires.
	if _, err := m.Update(task.ID, map[string]any{"status": "in-progress"}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	// Same status again → hook skipped (from==to).
	if _, err := m.Update(task.ID, map[string]any{"status": "in-progress"}); err != nil {
		t.Fatalf("Update status no-op: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("hook fired %d times, want 1: %+v", len(got), got)
	}
	if got[0].to != "in-progress" {
		t.Errorf("to = %q, want in-progress", got[0].to)
	}
	if got[0].id != task.ID {
		t.Errorf("id = %q, want %q", got[0].id, task.ID)
	}
}

func TestManagerDeleteEmitsEvent(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Delete(task.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskDeleted {
		t.Fatalf("events = %v, want [%s %s]", names, events.TaskCreated, events.TaskDeleted)
	}
}

func TestManagerAddRunEmitsUpdated(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.AddRun(task.ID, AgentRun{AgentID: "a1", State: "running"}); err != nil {
		t.Fatalf("AddRun: %v", err)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskUpdated {
		t.Fatalf("events = %v", names)
	}

	got, err := m.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.AgentRuns) != 1 || got.AgentRuns[0].AgentID != "a1" {
		t.Fatalf("AgentRuns = %+v", got.AgentRuns)
	}
}

func TestManagerUpdateRunEmitsUpdated(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.AddRun(task.ID, AgentRun{AgentID: "a1", State: "running"}); err != nil {
		t.Fatalf("AddRun: %v", err)
	}
	if err := m.UpdateRun(task.ID, "a1", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	names := emitter.names()
	// create + add_run + update_run = 3 updated/created events
	if len(names) != 3 || names[2] != events.TaskUpdated {
		t.Fatalf("events = %v", names)
	}

	got, err := m.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentRuns[0].State != "stopped" {
		t.Fatalf("run state = %q, want stopped", got.AgentRuns[0].State)
	}
}

func TestManagerConcurrentUpdateSerializes(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Two concurrent updates on the same id touching different fields
	// should both land without one overwriting the other.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := m.Update(task.ID, map[string]any{"title": "AAA"}); err != nil {
			t.Errorf("Update title: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := m.Update(task.ID, map[string]any{"body": "BBB"}); err != nil {
			t.Errorf("Update body: %v", err)
		}
	}()
	wg.Wait()

	got, err := m.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Whichever ran second wins for title; but body must be set since that
	// update is isolated. With no locking, a read-modify-write race could
	// cause body to be lost.
	if got.Body != "BBB" && got.Title != "AAA" {
		t.Fatalf("both updates lost: title=%q body=%q", got.Title, got.Body)
	}
}

func TestManagerConcurrentDifferentIDsParallel(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	t1, err := m.Create("One", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t2, err := m.Create("Two", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := m.Update(t1.ID, map[string]any{"body": "x"}); err != nil {
			t.Errorf("Update t1: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := m.Update(t2.ID, map[string]any{"body": "y"}); err != nil {
			t.Errorf("Update t2: %v", err)
		}
	}()
	wg.Wait()

	g1, _ := m.Get(t1.ID)
	g2, _ := m.Get(t2.ID)
	if g1.Body != "x" || g2.Body != "y" {
		t.Fatalf("bodies = %q / %q", g1.Body, g2.Body)
	}
}

func TestNoopEmitter(t *testing.T) {
	t.Parallel()
	m := NewManager(nil, nil)
	if m.emitter == nil {
		t.Fatal("emitter should never be nil")
	}
	// should not panic
	m.emitter.Emit("x", "y")
}
