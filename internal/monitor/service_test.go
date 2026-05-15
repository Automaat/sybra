package monitor

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/task"
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

func (f *fakeDispatcher) Dispatchable(Anomaly) (ok bool, skipReason string) { return true, "" }

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

var errNotFound = strError("not found")

type strError string

func (e strError) Error() string { return string(e) }

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

func TestServiceTick_PlanReviewStuck_RemediatesDirectly(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		// plan-review stuck: service must set it to human-required in-process.
		mkTask("pr-stuck", task.StatusPlanReview, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		}),
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

	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindStuckHumanBlocked {
		t.Fatalf("want 1 stuck_human_blocked anomaly, got %v", report.Anomalies)
	}

	// Remediator must have set the task to human-required.
	if len(tasks.updates) != 1 {
		t.Fatalf("want 1 task update, got %d", len(tasks.updates))
	}
	u := tasks.updates[0]
	if u.id != "pr-stuck" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status == nil || *u.u.Status != task.StatusHumanRequired {
		t.Errorf("status = %v, want human-required", u.u.Status)
	}

	// Sink must NOT receive the plan-review anomaly — no meta-task created.
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see plan-review anomaly, got %d submissions", len(sink.submissions))
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called for plan-review anomaly, got %d calls", len(disp.calls))
	}

	if len(report.Remediated) != 1 {
		t.Fatalf("want 1 remediated, got %d", len(report.Remediated))
	}
}

func TestServiceTick_HumanRequiredStuck_RemediatesDirectly(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		// human-required stuck with human-review verdict: remediated in-process.
		mkTask("hr-stuck", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{{
				Role:    "human-review",
				State:   "stopped",
				Verdict: "human",
			}}
		}),
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

	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindStuckHumanBlocked {
		t.Fatalf("want 1 stuck_human_blocked anomaly, got %v", report.Anomalies)
	}

	// Remediator must have refreshed the status reason (no status change).
	if len(tasks.updates) != 1 {
		t.Fatalf("want 1 task update, got %d", len(tasks.updates))
	}
	u := tasks.updates[0]
	if u.id != "hr-stuck" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status != nil {
		t.Errorf("status must not change for human-required stuck, got %v", u.u.Status)
	}
	if u.u.StatusReason != nil {
		t.Errorf("status_reason must not change, got %q", *u.u.StatusReason)
	}

	// Sink must NOT receive the anomaly — no meta-task created.
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see human-required anomaly, got %d submissions", len(sink.submissions))
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called for human-required anomaly, got %d calls", len(disp.calls))
	}

	if len(report.Remediated) != 1 {
		t.Fatalf("want 1 remediated, got %d", len(report.Remediated))
	}
}

// TestServiceTick_HumanRequiredStuck_FailedRunKeepsVerdict verifies that a
// failed latest human-review run (e.g. 529 with no parsable result) does NOT
// mask the "human" verdict from an earlier stopped run. The detector scans
// runs newest-to-oldest, so the verdict stands: RequiresLLM=false and the
// anomaly is remediated directly rather than dispatched to an LLM.
func TestServiceTick_HumanRequiredStuck_FailedRunKeepsVerdict(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		// Two human-review runs: older ended with Verdict="human", latest
		// ended with an unparsable 529 result. The earlier verdict must win.
		mkTask("hr-stale", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{
				{Role: "human-review", State: "stopped", Verdict: "human"},
				{Role: "human-review", State: "stopped", Result: "HTTP 529 service overloaded"},
			}
		}),
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

	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindStuckHumanBlocked {
		t.Fatalf("want 1 stuck_human_blocked anomaly, got %v", report.Anomalies)
	}
	// Failed last run must not mask the earlier "human" verdict.
	if report.Anomalies[0].RequiresLLM {
		t.Error("RequiresLLM must be false: earlier human verdict still applies")
	}

	// Deterministic verdict → remediated directly, dispatcher untouched.
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called, got %d calls", len(disp.calls))
	}
	if len(report.Remediated) != 1 {
		t.Fatalf("want 1 remediated, got %d: %v", len(report.Remediated), report.Remediated)
	}
}

// TestServiceTick_HumanRequiredStuck_DowngradedLLM covers the work-typed-task
// path: detector emits RequiresLLM=true (no human-review verdict yet), but
// DowngradeLLMForTask forces it to false. The remediation must still dwell-reset
// the task without dispatching an LLM agent or filing an issue.
func TestServiceTick_HumanRequiredStuck_DowngradedLLM_RemediatesDirectly(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		// human-required with no human-review run → RequiresLLM=true from detector.
		mkTask("hr-work", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
		}),
	}}
	disp := &fakeDispatcher{}
	sink := &fakeSink{createNext: true}
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		// Simulate DowngradeLLMForTask returning true for work-typed tasks.
		DowngradeLLMForTask: func(taskID string) bool { return taskID == "hr-work" },
		Dispatcher:          disp,
		Sink:                sink,
		Logger:              slog.Default(),
		Now:                 func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindStuckHumanBlocked {
		t.Fatalf("want 1 stuck_human_blocked anomaly, got %v", report.Anomalies)
	}
	if report.Anomalies[0].RequiresLLM {
		t.Error("RequiresLLM must be false after DowngradeLLMForTask")
	}

	// Dwell reset: one empty update, status and status_reason unchanged.
	if len(tasks.updates) != 1 {
		t.Fatalf("want 1 task update (dwell reset), got %d", len(tasks.updates))
	}
	u := tasks.updates[0]
	if u.id != "hr-work" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status != nil {
		t.Errorf("status must not change, got %v", u.u.Status)
	}
	if u.u.StatusReason != nil {
		t.Errorf("status_reason must not change, got %q", *u.u.StatusReason)
	}

	// No LLM dispatch and no issue filed for downgraded work-typed tasks.
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called for downgraded anomaly, got %d calls", len(disp.calls))
	}
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see downgraded human-required anomaly, got %d submissions", len(sink.submissions))
	}

	if len(report.Remediated) != 1 {
		t.Fatalf("want 1 remediated, got %d", len(report.Remediated))
	}
}

// TestServiceTick_HumanRequiredStuck_ResultFallback covers the scenario that
// produced a spurious meta-task for task af1213a4: single human-review run
// completed with the verdict embedded in the Result text, Verdict field empty
// (pre-Verdict-field era). The remediator must handle it in-process — no
// meta-task, no dispatcher call.
func TestServiceTick_HumanRequiredStuck_ResultFallback(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		mkTask("hr-result-fallback", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.AgentRuns = []task.AgentRun{{
				Role:  "human-review",
				State: "stopped",
				// Verdict field empty — decision must be parsed from Result.
				Result: "Analysis.\n\n```sybra-verdict\n{\"decision\":\"human\",\"summary\":\"budget exhausted\"}\n```",
			}}
		}),
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

	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindStuckHumanBlocked {
		t.Fatalf("want 1 stuck_human_blocked anomaly, got %v", report.Anomalies)
	}
	if report.Anomalies[0].RequiresLLM {
		t.Error("RequiresLLM must be false when Result contains a human verdict")
	}

	// Remediator must have stamped UpdatedAt (empty update, no status change).
	if len(tasks.updates) != 1 {
		t.Fatalf("want 1 task update (dwell reset), got %d", len(tasks.updates))
	}
	u := tasks.updates[0]
	if u.id != "hr-result-fallback" {
		t.Errorf("updated wrong task: %q", u.id)
	}
	if u.u.Status != nil {
		t.Errorf("status must not change, got %v", u.u.Status)
	}

	// Sink and dispatcher must not fire — no meta-task.
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see human-required anomaly, got %d submissions", len(sink.submissions))
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called, got %d calls", len(disp.calls))
	}

	if len(report.Remediated) != 1 {
		t.Fatalf("want 1 remediated, got %d", len(report.Remediated))
	}
}

func TestParseFirstMatchingIssue(t *testing.T) {
	out := []byte(`[{"number":42,"title":"unrelated","url":"https://github.com/o/r/issues/42"},{"number":87,"title":"[monitor] failure_spike","url":"https://github.com/o/r/issues/87"}]`)
	num, url := parseFirstMatchingIssue(out, "[monitor] failure_spike")
	if num != 87 {
		t.Errorf("want number 87, got %d", num)
	}
	if url != "https://github.com/o/r/issues/87" {
		t.Errorf("want url=.../issues/87, got %q", url)
	}
	if n, _ := parseFirstMatchingIssue(out, "no such title"); n != 0 {
		t.Errorf("expected zero number for no match")
	}
	if n, _ := parseFirstMatchingIssue([]byte("[]"), "anything"); n != 0 {
		t.Errorf("expected zero number for empty array")
	}
	// Backward-compat: number-only input (no url field) still matches.
	bareOut := []byte(`[{"number":99,"title":"[monitor] failure_spike"}]`)
	num2, url2 := parseFirstMatchingIssue(bareOut, "[monitor] failure_spike")
	if num2 != 99 || url2 != "" {
		t.Errorf("bare input: want (99, \"\"), got (%d, %q)", num2, url2)
	}
}
