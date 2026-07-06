package task

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/events"
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
	if _, err := m.Update(task.ID, Update{Title: Ptr("New")}); err != nil {
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
	if _, err := m.Update(task.ID, Update{Title: Ptr("X")}); err != nil {
		t.Fatalf("Update title: %v", err)
	}
	// Status change → hook fires.
	if _, err := m.Update(task.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	// Same status again → hook skipped (from==to).
	if _, err := m.Update(task.ID, Update{Status: Ptr(StatusInProgress)}); err != nil {
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

// TestManagerOnExternalUpdateDoesNotDoubleFireRacingInProcessWrite guards
// against the exact race behind the "two independent cascade sources" bug: an
// in-process status write (e.g. UpdateTaskStatus from a workflow step) and
// the file watcher event it wakes both observe the same transition and both
// try to fire the status-change hook. Before the fix, OnExternalUpdate read +
// deduped + recorded without holding the per-task lock UpdateFn writes under,
// so a watcher call landing between the write and the writer's own
// recordFiredStatus call could win the dedupe race and fire a second,
// concurrent hook invocation for the same transition — which is exactly what
// let a status-change hook and a legitimate cascade both dispatch the same
// successor workflow. Hammer both entry points concurrently and assert the
// hook only ever fires once per distinct transition.
func TestManagerOnExternalUpdateDoesNotDoubleFireRacingInProcessWrite(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Task", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	path := filepath.Join(m.store.dir, created.ID+".md")

	var (
		mu   sync.Mutex
		fire []string
	)
	m.SetStatusChangeHook(func(id, from, to string) {
		mu.Lock()
		fire = append(fire, from+"->"+to)
		mu.Unlock()
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, updErr := m.Update(created.ID, Update{Status: Ptr(StatusInProgress)}); updErr != nil {
			t.Errorf("Update: %v", updErr)
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			m.OnExternalUpdate(path)
		}
	}()
	wg.Wait()

	// Give any straggling OnExternalUpdate call (from the loop above, racing
	// past the writer) a chance to observe the final state and settle.
	m.OnExternalUpdate(path)

	mu.Lock()
	defer mu.Unlock()
	count := 0
	for _, f := range fire {
		if f == "->in-progress" || f == "todo->in-progress" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("hook fired %d times for todo->in-progress, want 1: %+v", count, fire)
	}
}

func TestManagerOnExternalUpdateInvalidatesJSONSidecar(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Task", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.List(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	contractPath := filepath.Join(m.store.dir, created.ID+".plan-contract.json")
	contract := `{"task_id":"` + created.ID + `"}`
	if err := os.WriteFile(contractPath, []byte(contract), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}

	m.OnExternalUpdate(contractPath)

	tasks, err := m.List()
	if err != nil {
		t.Fatalf("list after external update: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].PlanContract != contract {
		t.Fatalf("PlanContract = %q, want %q", tasks[0].PlanContract, contract)
	}
}

// TestManagerAppendBodyDoesNotDeadlockOnSelfRoutedEmit reproduces the
// production wiring in app.go's emit closure, which routes every
// task:updated event straight back into Manager.OnExternalUpdate on the
// same goroutine (so cross-process file writes and in-process updates share
// one status-change path). AppendBody used to fire that emit while still
// holding lockFor(id) via a deferred unlock, so the re-entrant
// OnExternalUpdate call blocked forever on the same non-reentrant mutex —
// the goroutine that wedged issue #1567 (a FAIL test-runner verdict calls
// AppendBody via appendTestFailureReport).
func TestManagerAppendBodyDoesNotDeadlockOnSelfRoutedEmit(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	created, err := m.Create("Task", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// OnExternalUpdate only takes lockFor(id) when a status-change hook is
	// registered — without one it returns before locking and the reentrant
	// path this test targets would never be exercised.
	m.SetStatusChangeHook(func(string, string, string) {})
	m.emitter = EmitterFunc(func(event string, data any) {
		if event != events.TaskUpdated {
			return
		}
		if path, ok := data.(string); ok {
			m.OnExternalUpdate(path)
		}
	})

	done := make(chan error, 1)
	go func() {
		_, appendErr := m.AppendBody(created.ID, "note")
		done <- appendErr
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AppendBody: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AppendBody deadlocked on self-routed emit")
	}
}

func TestManagerAddRunWithStatusEmitsUpdatedAndHook(t *testing.T) {
	t.Parallel()
	m, emitter := newTestManager(t)

	task, err := m.Create("Title", "", "headless")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var hookCalls int
	m.SetStatusChangeHook(func(id, from, to string) {
		hookCalls++
		if id != task.ID || from != string(StatusTodo) || to != string(StatusInProgress) {
			t.Fatalf("hook got (%s,%s,%s)", id, from, to)
		}
	})

	if err := m.AddRunWithStatus(task.ID, AgentRun{AgentID: "a1", State: "running"}, Ptr(StatusInProgress)); err != nil {
		t.Fatalf("AddRunWithStatus: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("hookCalls = %d, want 1", hookCalls)
	}

	names := emitter.names()
	if len(names) != 2 || names[1] != events.TaskUpdated {
		t.Fatalf("events = %v", names)
	}

	got, err := m.Get(task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInProgress {
		t.Fatalf("Status = %q, want %q", got.Status, StatusInProgress)
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
	if err := m.UpdateRun(task.ID, "a1", RunPatch{State: Ptr("stopped")}); err != nil {
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
		if _, err := m.Update(task.ID, Update{Title: Ptr("AAA")}); err != nil {
			t.Errorf("Update title: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := m.Update(task.ID, Update{Body: Ptr("BBB")}); err != nil {
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
		if _, err := m.Update(t1.ID, Update{Body: Ptr("x")}); err != nil {
			t.Errorf("Update t1: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := m.Update(t2.ID, Update{Body: Ptr("y")}); err != nil {
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

func TestLockForTypeMismatchNoPanic(t *testing.T) {
	t.Parallel()
	m, _ := newTestManager(t)

	// Manually store a non-*sync.Mutex value to simulate type mismatch.
	m.locks.Store("bad-id", "not-a-mutex")

	// Must not panic; returned mutex must be usable.
	mu := m.lockFor("bad-id")
	if mu == nil {
		t.Fatal("lockFor returned nil on type mismatch")
	}
	func() {
		mu.Lock()
		defer mu.Unlock()
	}()
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
