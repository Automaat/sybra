package monitor

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/task"
)

type fakeTasks struct {
	mu      sync.Mutex
	tasks   []task.Task
	updates []taskUpdate
}

type taskUpdate struct {
	id string
	u  task.Update
}

func (f *fakeTasks) List() ([]task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]task.Task, len(f.tasks))
	copy(out, f.tasks)
	return out, nil
}

func (f *fakeTasks) Get(id string) (task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.tasks {
		if f.tasks[i].ID == id {
			return f.tasks[i], nil
		}
	}
	return task.Task{}, errNotFound
}

func (f *fakeTasks) Update(id string, u task.Update) (task.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, taskUpdate{id: id, u: u})
	for i := range f.tasks {
		if f.tasks[i].ID != id {
			continue
		}
		if u.Status != nil {
			f.tasks[i].Status = *u.Status
		}
		if u.StatusReason != nil {
			f.tasks[i].StatusReason = *u.StatusReason
		}
		return f.tasks[i], nil
	}
	return task.Task{}, errNotFound
}

type fakeAudit struct {
	events []audit.Event
}

func (f fakeAudit) Read(audit.Query) ([]audit.Event, error) {
	out := make([]audit.Event, len(f.events))
	copy(out, f.events)
	return out, nil
}

// nilAgentLister is what the service uses when no live-agent suppression is
// needed. Returning nil from ListAgents is safe because snapshotLiveAgents
// guards.
type nilAgentLister struct{}

func (nilAgentLister) ListAgents() []*agent.Agent { return nil }

// liveAgentLister returns a fixed set of running agents; used to suppress
// false-positive lost_agent flags in tests that care about other anomalies.
type liveAgentLister struct {
	taskIDs []string
}

func (l liveAgentLister) ListAgents() []*agent.Agent {
	out := make([]*agent.Agent, 0, len(l.taskIDs))
	for _, id := range l.taskIDs {
		a := &agent.Agent{TaskID: id}
		a.SetState(agent.StateRunning)
		out = append(out, a)
	}
	return out
}

type fakeDispatcher struct {
	mu     sync.Mutex
	calls  []Anomaly
	nextID int
}

func (f *fakeDispatcher) Dispatch(_ context.Context, a Anomaly) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.calls = append(f.calls, a)
	return "agent-1", nil
}

type fakeSink struct {
	mu          sync.Mutex
	submissions []Anomaly
	createNext  bool
}

func (f *fakeSink) Submit(_ context.Context, a Anomaly, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submissions = append(f.submissions, a)
	return f.createNext, nil
}

var errNotFound = errStr("not found")

type errStr string

func (e errStr) Error() string { return string(e) }

func TestServiceTickEndToEnd(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		// Lost agent: in-progress, no live agent, no audit event.
		mkTask("lost", task.StatusInProgress),
		// Untriaged: todo without tags or mode.
		mkTask("untriaged", task.StatusTodo, func(t *task.Task) {
			t.Tags = nil
			t.AgentMode = ""
		}),
		// PR gap: in-review with project but no PR.
		mkTask("pr", task.StatusInReview, func(t *task.Task) { t.ProjectID = "owner/repo" }),
	}}
	disp := &fakeDispatcher{}
	sink := &fakeSink{createNext: true}
	svc := NewService(Deps{
		Cfg:        cfg,
		Tasks:      tasks,
		Audit:      fakeAudit{},
		Agents:     nilAgentLister{},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	wantKinds := map[AnomalyKind]bool{
		KindLostAgent: true,
		KindUntriaged: true,
		KindPRGap:     true,
	}
	if len(report.Anomalies) != 3 {
		t.Fatalf("want 3 anomalies, got %d (%v)", len(report.Anomalies), report.Anomalies)
	}
	for _, a := range report.Anomalies {
		if !wantKinds[a.Kind] {
			t.Errorf("unexpected anomaly: %s", a.Kind)
		}
	}

	// Idempotent remediations should have updated lost + untriaged.
	if got := len(tasks.updates); got != 2 {
		t.Fatalf("want 2 task updates, got %d (%v)", got, tasks.updates)
	}

	// pr_gap is RequiresLLM=true → must be dispatched.
	if got := len(disp.calls); got != 1 {
		t.Fatalf("want 1 dispatch, got %d", got)
	}
	if disp.calls[0].Kind != KindPRGap {
		t.Errorf("dispatched wrong kind: %s", disp.calls[0].Kind)
	}

	// Both deterministic anomalies should have been submitted to the sink
	// (lost_agent and untriaged). pr_gap was dispatched so its issue is the
	// LLM agent's responsibility — sink should not see it.
	if got := len(sink.submissions); got != 2 {
		t.Fatalf("want 2 sink submissions, got %d", got)
	}
	for _, a := range sink.submissions {
		if a.Kind == KindPRGap {
			t.Errorf("sink got pr_gap: should be dispatched, not filed deterministically")
		}
	}
	if report.IssuesOpened != 2 || report.IssuesUpdated != 0 {
		t.Errorf("want issuesOpened=2 issuesUpdated=0, got %d/%d", report.IssuesOpened, report.IssuesUpdated)
	}
}

func TestServiceCooldownSuppressesSecondTick(t *testing.T) {
	base := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	cfg.IssueCooldownMinutes = 30
	cfg.DispatchLimit = 1
	// over_dispatch_limit fires deterministically (no remediation), so tick N
	// always re-detects the same anomaly and the cooldown is the only reason
	// the issue sink doesn't double-submit.
	tasks := &fakeTasks{tasks: []task.Task{
		mkTask("a", task.StatusInProgress),
		mkTask("b", task.StatusInProgress),
	}}
	sink := &fakeSink{createNext: true}
	disp := &fakeDispatcher{}

	now := base
	svc := NewService(Deps{
		Cfg:        cfg,
		Tasks:      tasks,
		Audit:      fakeAudit{},
		Agents:     liveAgentLister{taskIDs: []string{"a", "b"}},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
	})

	if _, err := svc.tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if got := len(sink.submissions); got != 1 {
		t.Fatalf("tick1 want 1 submission, got %d", got)
	}

	// Second tick within cooldown should not re-submit.
	now = base.Add(5 * time.Minute)
	if _, err := svc.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if got := len(sink.submissions); got != 1 {
		t.Fatalf("tick2 (within cooldown) want 1 submission total, got %d", got)
	}

	// Third tick after cooldown should re-submit.
	now = base.Add(40 * time.Minute)
	if _, err := svc.tick(context.Background()); err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if got := len(sink.submissions); got != 2 {
		t.Fatalf("tick3 (after cooldown) want 2 submissions total, got %d", got)
	}
}

func TestServiceScanHasNoSideEffects(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	tasks := &fakeTasks{tasks: []task.Task{mkTask("lost", task.StatusInProgress)}}
	sink := &fakeSink{createNext: true}
	disp := &fakeDispatcher{}
	svc := NewService(Deps{
		Cfg:        defaultCfg(),
		Tasks:      tasks,
		Audit:      fakeAudit{},
		Agents:     nilAgentLister{},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
	})
	r, err := svc.Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(r.Anomalies) != 1 {
		t.Fatalf("want 1 anomaly, got %d", len(r.Anomalies))
	}
	if len(tasks.updates) != 0 || len(sink.submissions) != 0 || len(disp.calls) != 0 {
		t.Errorf("scan must not produce side effects (updates=%d sink=%d dispatch=%d)",
			len(tasks.updates), len(sink.submissions), len(disp.calls))
	}
}

func TestParseFirstMatchingIssueNumber(t *testing.T) {
	out := []byte(`[{"number":42,"title":"unrelated"},{"number":87,"title":"[monitor] failure_spike"}]`)
	got := parseFirstMatchingIssueNumber(out, "[monitor] failure_spike")
	if got != 87 {
		t.Errorf("want 87, got %d", got)
	}
	if parseFirstMatchingIssueNumber(out, "no such title") != 0 {
		t.Errorf("expected zero for no match")
	}
	if parseFirstMatchingIssueNumber([]byte("[]"), "anything") != 0 {
		t.Errorf("expected zero for empty array")
	}
}
