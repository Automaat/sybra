package monitor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

type fakeTasks struct {
	mu         sync.Mutex
	tasks      []task.Task
	updates    []taskUpdate
	runUpdates []runUpdate
	// updateErr, when non-nil, is returned by Update for this id instead of
	// applying the update — simulates a task-store write conflict.
	updateErr map[string]error
}

type taskUpdate struct {
	id string
	u  task.Update
}

type runUpdate struct {
	taskID  string
	agentID string
	patch   task.RunPatch
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
	if err := f.updateErr[id]; err != nil {
		return task.Task{}, err
	}
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
		if u.Outcome != nil {
			f.tasks[i].Outcome = *u.Outcome
		}
		if u.PRNumber != nil {
			f.tasks[i].PRNumber = *u.PRNumber
		}
		if u.Tags != nil {
			f.tasks[i].Tags = append([]string(nil), (*u.Tags)...)
		}
		if u.EffectLog != nil {
			f.tasks[i].EffectLog = append([]workflow.EffectRecord(nil), (*u.EffectLog)...)
		}
		if u.Workflow != nil {
			f.tasks[i].Workflow = *u.Workflow
		}
		return f.tasks[i], nil
	}
	return task.Task{}, errNotFound
}

func (f *fakeTasks) ApplyStatusEffect(id string, eff task.StatusEffect) (task.Task, error) {
	u := eff.Extra
	u.Status = &eff.ToStatus
	return f.Update(id, u)
}

func (f *fakeTasks) UpdateRun(taskID, agentID string, patch task.RunPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runUpdates = append(f.runUpdates, runUpdate{taskID: taskID, agentID: agentID, patch: patch})
	for i := range f.tasks {
		if f.tasks[i].ID != taskID {
			continue
		}
		for j := range f.tasks[i].AgentRuns {
			if f.tasks[i].AgentRuns[j].AgentID != agentID {
				continue
			}
			if patch.State != nil {
				f.tasks[i].AgentRuns[j].State = *patch.State
			}
			return nil
		}
		return nil
	}
	return errNotFound
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
	bodies      []string
	createNext  bool
	closed      []Anomaly
	closeNext   bool
	closeErr    error
}

func (f *fakeSink) Submit(_ context.Context, a Anomaly, body string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submissions = append(f.submissions, a)
	f.bodies = append(f.bodies, body)
	return f.createNext, nil
}

// CloseIfOpen implements IssueCloser so tests can exercise the auto-close
// path without a real gh binary.
func (f *fakeSink) CloseIfOpen(_ context.Context, a Anomaly, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, a)
	if f.closeErr != nil {
		return false, f.closeErr
	}
	return f.closeNext, nil
}

// toggleAgentLister is a mutable agentLister: tests flip which task ids are
// "live" between tick() calls to simulate an agent recovering mid-run,
// something a fixed liveAgentLister can't express since it's wired once at
// NewService time.
type toggleAgentLister struct {
	mu      sync.Mutex
	taskIDs []string
}

func (l *toggleAgentLister) Set(taskIDs []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.taskIDs = taskIDs
}

func (l *toggleAgentLister) ListAgents() []*agent.Agent {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*agent.Agent, 0, len(l.taskIDs))
	for _, id := range l.taskIDs {
		a := &agent.Agent{TaskID: id}
		a.SetState(agent.StateRunning)
		out = append(out, a)
	}
	return out
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
		mkTask("pr", task.StatusInReview, func(t *task.Task) {
			t.ProjectID = "owner/repo"
			t.UpdatedAt = now.Add(-20 * time.Minute)
		}),
	}}
	disp := &fakeDispatcher{}
	sink := &fakeSink{createNext: true}
	var recoverCalls int
	svc := NewService(Deps{
		Cfg:        cfg,
		Tasks:      tasks,
		Audit:      fakeAudit{},
		Agents:     nilAgentLister{},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
		RecoverLostAgent: func(context.Context, string) {
			recoverCalls++
		},
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
	if recoverCalls != 1 {
		t.Fatalf("want 1 lost-agent recovery call, got %d", recoverCalls)
	}

	// pr_gap is RequiresLLM=true → must be dispatched.
	if got := len(disp.calls); got != 1 {
		t.Fatalf("want 1 dispatch, got %d", got)
	}
	if disp.calls[0].Kind != KindPRGap {
		t.Errorf("dispatched wrong kind: %s", disp.calls[0].Kind)
	}

	// lost_agent was recovered in-process, so only untriaged should still be
	// filed deterministically. pr_gap was dispatched so its issue is the LLM
	// agent's responsibility — sink should not see it.
	if got := len(sink.submissions); got != 1 {
		t.Fatalf("want 1 sink submission, got %d", got)
	}
	for _, a := range sink.submissions {
		if a.Kind == KindLostAgent {
			t.Errorf("sink got lost_agent: recovered anomalies must not still file an issue")
		}
		if a.Kind == KindPRGap {
			t.Errorf("sink got pr_gap: should be dispatched, not filed deterministically")
		}
	}
	if report.IssuesOpened != 1 || report.IssuesUpdated != 0 {
		t.Errorf("want issuesOpened=1 issuesUpdated=0, got %d/%d", report.IssuesOpened, report.IssuesUpdated)
	}
}

func TestServiceTickClosesHumanRequiredTaskWithMergedPR(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		mkTask("merged", task.StatusHumanRequired, func(t *task.Task) {
			t.ProjectID = "owner/repo"
			t.PRNumber = 42
			t.UpdatedAt = now.Add(-time.Minute)
		}),
	}}
	var fetched int
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.DiscardHandler),
		FetchPRState: func(repo string, number int) (github.PRState, error) {
			fetched++
			if repo != "owner/repo" || number != 42 {
				t.Fatalf("FetchPRState(%q, %d), want owner/repo#42", repo, number)
			}
			return github.PRState{State: "MERGED"}, nil
		},
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fetched != 1 {
		t.Fatalf("FetchPRState calls = %d, want 1", fetched)
	}
	got, err := tasks.Get("merged")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if len(report.Remediated) != 1 || report.Remediated[0] != "linked_pr_merged:merged" {
		t.Fatalf("remediated = %v, want [linked_pr_merged:merged]", report.Remediated)
	}
}

func TestServiceTick_LostAgentRecoverySuppressesIssue(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	tasks := &fakeTasks{tasks: []task.Task{mkTask("lost", task.StatusInProgress)}}
	sink := &fakeSink{createNext: true}
	var recoverCalls int
	svc := NewService(Deps{
		Cfg:    defaultCfg(),
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		Sink:   sink,
		Logger: slog.Default(),
		Now:    func() time.Time { return now },
		RecoverLostAgent: func(context.Context, string) {
			recoverCalls++
		},
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if recoverCalls != 1 {
		t.Fatalf("want 1 lost-agent recovery call, got %d", recoverCalls)
	}
	if len(report.Remediated) != 1 || report.Remediated[0] != "lost_agent:lost" {
		t.Fatalf("remediated = %v, want [lost_agent:lost]", report.Remediated)
	}
	if len(sink.submissions) != 0 {
		t.Fatalf("want 0 sink submissions after lost-agent recovery, got %d", len(sink.submissions))
	}
	if report.IssuesOpened != 0 || report.IssuesUpdated != 0 {
		t.Fatalf("want issuesOpened=0 issuesUpdated=0, got %d/%d", report.IssuesOpened, report.IssuesUpdated)
	}
}

// TestServiceTick_LostAgentDedupeThenAutoClose is the scenario from #2497's
// test approach: repeated lost-agent detection on one task must reuse a
// single open issue (comment with an occurrence count, not a fresh filing
// every tick), and once the task's agent recovers and stays clear for
// LostAgentAutoCloseAfterClears consecutive ticks, the issue auto-closes.
func TestServiceTick_LostAgentDedupeThenAutoClose(t *testing.T) {
	base := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	cfg.IssueCooldownMinutes = 0 // isolate the occurrence gate from the cooldown gate
	cfg.LostAgentIssueAfterOccurrences = 2
	cfg.LostAgentAutoCloseAfterClears = 2

	tasks := &fakeTasks{tasks: []task.Task{mkTask("flaky", task.StatusInProgress)}}
	sink := &fakeSink{createNext: true}
	agents := &toggleAgentLister{}
	now := base
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: agents,
		Sink:   sink,
		Logger: slog.Default(),
		Now:    func() time.Time { return now },
	})

	tick := func(step time.Duration) Report {
		now = now.Add(step)
		r, err := svc.tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		return r
	}

	// Occurrence 1: first detection, remediation hasn't had a chance yet —
	// no issue filed.
	r := tick(0)
	if len(sink.submissions) != 0 {
		t.Fatalf("occurrence 1: want 0 submissions, got %d", len(sink.submissions))
	}
	if r.IssuesOpened != 0 || r.IssuesUpdated != 0 {
		t.Fatalf("occurrence 1: want 0 opened/updated, got %d/%d", r.IssuesOpened, r.IssuesUpdated)
	}

	// Occurrence 2: recurred despite remediation — files the one issue.
	r = tick(15 * time.Minute)
	if len(sink.submissions) != 1 {
		t.Fatalf("occurrence 2: want 1 submission, got %d", len(sink.submissions))
	}
	if r.IssuesOpened != 1 || r.IssuesUpdated != 0 {
		t.Fatalf("occurrence 2: want 1 opened, 0 updated, got %d/%d", r.IssuesOpened, r.IssuesUpdated)
	}
	sink.createNext = false // simulate: the issue now exists, further hits are comments

	// Occurrence 3: still stuck — reuses the same issue via an occurrence
	// comment, not a fresh filing.
	r = tick(15 * time.Minute)
	if len(sink.submissions) != 2 {
		t.Fatalf("occurrence 3: want 2 submissions total, got %d", len(sink.submissions))
	}
	if r.IssuesOpened != 0 || r.IssuesUpdated != 1 {
		t.Fatalf("occurrence 3: want 0 opened, 1 updated, got %d/%d", r.IssuesOpened, r.IssuesUpdated)
	}
	lastBody := sink.bodies[len(sink.bodies)-1]
	if !strings.Contains(lastBody, "occurrence #3") {
		t.Fatalf("occurrence 3: want comment body to reference occurrence #3, got %q", lastBody)
	}
	if strings.Contains(lastBody, "## Suggested investigation") {
		t.Fatalf("occurrence 3: want a terse recurrence comment, not the full deterministic body: %q", lastBody)
	}

	// Recovery: the agent is now live, so lost_agent stops being detected.
	agents.Set([]string{"flaky"})

	// Clear 1 of 2 — not enough to auto-close yet.
	r = tick(15 * time.Minute)
	if len(sink.closed) != 0 {
		t.Fatalf("clear 1: want 0 close attempts, got %d", len(sink.closed))
	}
	if r.IssuesClosed != 0 {
		t.Fatalf("clear 1: want 0 issues closed, got %d", r.IssuesClosed)
	}

	// Clear 2 of 2 — condition has stayed clear long enough, auto-close.
	sink.closeNext = true
	r = tick(15 * time.Minute)
	if len(sink.closed) != 1 {
		t.Fatalf("clear 2: want 1 close attempt, got %d", len(sink.closed))
	}
	if r.IssuesClosed != 1 {
		t.Fatalf("clear 2: want 1 issue closed, got %d", r.IssuesClosed)
	}

	// A later, brand-new occurrence must start a clean streak rather than
	// instantly refiling against the now-closed issue.
	sink.submissions = nil
	agents.Set(nil)
	r = tick(15 * time.Minute)
	if len(sink.submissions) != 0 {
		t.Fatalf("post-close occurrence 1: want 0 submissions, got %d", len(sink.submissions))
	}
	if r.IssuesOpened != 0 {
		t.Fatalf("post-close occurrence 1: want 0 opened, got %d", r.IssuesOpened)
	}
}

// TestServiceTick_LostAgentAutoCloseRetriesAfterTransientFailure guards the
// #2497 fix: a transient CloseIfOpen error on the exact tick that crosses
// LostAgentAutoCloseAfterClears must NOT orphan the open issue. The tracking
// entry has to survive so a subsequent tick retries the close, since a
// stayed-clear condition never re-triggers a fresh detection.
func TestServiceTick_LostAgentAutoCloseRetriesAfterTransientFailure(t *testing.T) {
	base := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	cfg.IssueCooldownMinutes = 0
	cfg.LostAgentIssueAfterOccurrences = 1
	cfg.LostAgentAutoCloseAfterClears = 1

	tasks := &fakeTasks{tasks: []task.Task{mkTask("flaky", task.StatusInProgress)}}
	sink := &fakeSink{createNext: true}
	agents := &toggleAgentLister{}
	now := base
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: agents,
		Sink:   sink,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return now },
	})

	tick := func(step time.Duration) Report {
		now = now.Add(step)
		r, err := svc.tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		return r
	}

	// File the issue for the stuck task.
	tick(0)
	if len(sink.submissions) != 1 {
		t.Fatalf("want 1 submission after file, got %d", len(sink.submissions))
	}
	sink.createNext = false

	// Agent recovers; the first clear tick crosses the auto-close threshold
	// but CloseIfOpen fails transiently.
	agents.Set([]string{"flaky"})
	sink.closeErr = errNotFound
	r := tick(15 * time.Minute)
	if len(sink.closed) != 1 {
		t.Fatalf("transient tick: want 1 close attempt, got %d", len(sink.closed))
	}
	if r.IssuesClosed != 0 {
		t.Fatalf("transient tick: want 0 issues closed, got %d", r.IssuesClosed)
	}

	// The transient error cleared; the very next tick (still clear) must retry
	// the close instead of leaving the issue orphaned.
	sink.closeErr = nil
	sink.closeNext = true
	r = tick(15 * time.Minute)
	if len(sink.closed) != 2 {
		t.Fatalf("retry tick: want 2 close attempts total, got %d", len(sink.closed))
	}
	if r.IssuesClosed != 1 {
		t.Fatalf("retry tick: want 1 issue closed on retry, got %d", r.IssuesClosed)
	}

	// Once closed, the entry is forgotten: further clear ticks don't re-attempt.
	r = tick(15 * time.Minute)
	if len(sink.closed) != 2 {
		t.Fatalf("post-close tick: want no further close attempts, got %d", len(sink.closed))
	}
	if r.IssuesClosed != 0 {
		t.Fatalf("post-close tick: want 0 issues closed, got %d", r.IssuesClosed)
	}
}

func TestServiceTick_UntriagedAutoClosesAfterTriage(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	source := mkTaskAt(now, "source", task.StatusTodo, func(t *task.Task) {
		t.Tags = []string{"small"}
	})
	investigation := mkTaskAt(now, "investigation", task.StatusTodo, func(t *task.Task) {
		t.Tags = []string{string(task.FlagSybraBug), string(task.FlagScrubbed), untriagedInvestigationTag}
		t.Body = DeterministicIssueBody(Anomaly{
			Kind:        KindUntriaged,
			TaskID:      source.ID,
			Severity:    SeverityInfo,
			Fingerprint: Fingerprint(KindUntriaged, source.ID, nil),
			DetectedAt:  now.Add(-time.Hour),
		})
	})
	tasks := &fakeTasks{tasks: []task.Task{source, investigation}}
	sink := &fakeSink{closeNext: true}
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		Sink:   sink,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(sink.closed) != 1 {
		t.Fatalf("want 1 close attempt, got %d", len(sink.closed))
	}
	if sink.closed[0].Kind != KindUntriaged || sink.closed[0].TaskID != source.ID {
		t.Fatalf("closed = %+v, want untriaged for %q", sink.closed[0], source.ID)
	}
	if report.IssuesClosed != 1 {
		t.Fatalf("issuesClosed = %d, want 1", report.IssuesClosed)
	}
	if report.IssuesOpened != 0 || report.IssuesUpdated != 0 {
		t.Fatalf("want no issue submissions, got opened=%d updated=%d", report.IssuesOpened, report.IssuesUpdated)
	}
}

func TestServiceTick_UntriagedAutoClosesWhenSourceTaskDeleted(t *testing.T) {
	now := time.Date(2026, 7, 27, 18, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	investigation := mkTaskAt(now, "investigation", task.StatusTodo, func(t *task.Task) {
		t.Tags = []string{string(task.FlagSybraBug), string(task.FlagScrubbed), untriagedInvestigationTag}
		t.Body = DeterministicIssueBody(Anomaly{
			Kind:        KindUntriaged,
			TaskID:      "missing-source",
			Severity:    SeverityInfo,
			Fingerprint: Fingerprint(KindUntriaged, "missing-source", nil),
			DetectedAt:  now.Add(-time.Hour),
		})
	})
	tasks := &fakeTasks{tasks: []task.Task{investigation}}
	sink := &fakeSink{closeNext: true}
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		Sink:   sink,
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(sink.closed) != 1 {
		t.Fatalf("want 1 close attempt, got %d", len(sink.closed))
	}
	if sink.closed[0].TaskID != "missing-source" {
		t.Fatalf("closed task id = %q, want missing-source", sink.closed[0].TaskID)
	}
	if report.IssuesClosed != 1 {
		t.Fatalf("issuesClosed = %d, want 1", report.IssuesClosed)
	}
}

// TestServiceTick_LostAgentRemediationFailureFilesImmediately covers the
// other half of "file only after remediation has failed at least once": an
// outright remediation error (e.g. a task-store write conflict) must file on
// the very first tick, not wait for the occurrence-streak gate. It also
// checks that once the underlying resetLostAgent update starts succeeding
// again on a later tick (the write conflict clears) but the task is still
// stuck, the SAME already-open issue keeps getting a recurrence comment
// rather than a second, differently-fingerprinted one.
func TestServiceTick_LostAgentRemediationFailureFilesImmediately(t *testing.T) {
	base := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	cfg.IssueCooldownMinutes = 0

	tasks := &fakeTasks{
		tasks:     []task.Task{mkTask("conflict", task.StatusInProgress)},
		updateErr: map[string]error{"conflict": errNotFound},
	}
	sink := &fakeSink{createNext: true}
	now := base
	svc := NewService(Deps{
		Cfg:    cfg,
		Tasks:  tasks,
		Audit:  fakeAudit{},
		Agents: nilAgentLister{},
		Sink:   sink,
		Logger: slog.Default(),
		Now:    func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if len(sink.submissions) != 1 {
		t.Fatalf("tick1: want 1 submission on the very first tick, got %d", len(sink.submissions))
	}
	if report.IssuesOpened != 1 {
		t.Fatalf("tick1: want 1 issue opened, got %d", report.IssuesOpened)
	}
	filedFP := sink.submissions[0].Fingerprint
	if filedFP == Fingerprint(KindLostAgent, "conflict", nil) {
		t.Fatalf("tick1: want a cause-qualified fingerprint, got the bare base one %q", filedFP)
	}

	// The write conflict clears, but the task is still stuck — resetLostAgent
	// now "succeeds" every tick without actually fixing anything.
	tasks.updateErr = nil
	sink.createNext = false

	now = now.Add(15 * time.Minute)
	if _, err := svc.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sink.submissions) != 2 {
		t.Fatalf("tick2: want 2 submissions total, got %d", len(sink.submissions))
	}
	if got := sink.submissions[1].Fingerprint; got != filedFP {
		t.Fatalf("tick2: want the already-filed fingerprint %q reused, got %q", filedFP, got)
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
	var recoverCalls int
	svc := NewService(Deps{
		Cfg:        defaultCfg(),
		Tasks:      tasks,
		Audit:      fakeAudit{},
		Agents:     nilAgentLister{},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
		RecoverLostAgent: func(context.Context, string) {
			recoverCalls++
		},
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
	if recoverCalls != 0 {
		t.Fatalf("scan must not call lost-agent recovery, got %d calls", recoverCalls)
	}
}

func TestServiceTickObserverOnlyHasNoSideEffects(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	tasks := &fakeTasks{tasks: []task.Task{mkTask("lost", task.StatusInProgress)}}
	sink := &fakeSink{createNext: true}
	disp := &fakeDispatcher{}
	var recoverCalls int
	svc := NewService(Deps{
		Cfg:          defaultCfg(),
		Tasks:        tasks,
		Audit:        fakeAudit{},
		Agents:       nilAgentLister{},
		ObserverOnly: true,
		Dispatcher:   disp,
		Sink:         sink,
		Logger:       slog.Default(),
		Now:          func() time.Time { return now },
		RecoverLostAgent: func(context.Context, string) {
			recoverCalls++
		},
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(report.Anomalies) != 1 || report.Anomalies[0].Kind != KindLostAgent {
		t.Fatalf("want one lost_agent anomaly, got %v", report.Anomalies)
	}
	if len(report.Remediated) != 0 || len(report.Dispatched) != 0 || report.IssuesOpened != 0 || report.IssuesUpdated != 0 {
		t.Fatalf("observer-only tick must stay read-only, got remediated=%v dispatched=%v opened=%d updated=%d",
			report.Remediated, report.Dispatched, report.IssuesOpened, report.IssuesUpdated)
	}
	if len(tasks.updates) != 0 || len(tasks.runUpdates) != 0 || len(sink.submissions) != 0 || len(disp.calls) != 0 || recoverCalls != 0 {
		t.Fatalf("observer-only tick mutated state (updates=%d run_updates=%d sink=%d dispatch=%d recover=%d)",
			len(tasks.updates), len(tasks.runUpdates), len(sink.submissions), len(disp.calls), recoverCalls)
	}
}

// A plan-review task, however long it dwells, must not be flagged, remediated,
// dispatched, or filed — it waits indefinitely for the human's plan review.
func TestServiceTick_PlanReviewStuck_NotFlagged(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
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

	for _, a := range report.Anomalies {
		if a.Kind == KindStuckHumanBlocked {
			t.Fatalf("plan-review must not produce a stuck_human_blocked anomaly, got %v", report.Anomalies)
		}
	}
	if len(tasks.updates) != 0 {
		t.Fatalf("plan-review must be left untouched, got %d task updates", len(tasks.updates))
	}
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see plan-review, got %d submissions", len(sink.submissions))
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called for plan-review, got %d calls", len(disp.calls))
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

func TestServiceTick_HumanRequiredStuck_DowngradedLLM_MergedPRUsesLandingPipeline(t *testing.T) {
	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	cfg := defaultCfg()
	tasks := &fakeTasks{tasks: []task.Task{
		mkTask("hr-merged", task.StatusHumanRequired, func(t *task.Task) {
			t.UpdatedAt = now.Add(-9 * time.Hour)
			t.ProjectID = "o/r"
			t.PRNumber = 42
			t.Workflow = &workflow.Execution{
				WorkflowID:  "simple-task-review",
				CurrentStep: "wait_human",
				State:       workflow.ExecWaiting,
				Variables:   map[string]string{},
			}
		}),
	}}
	disp := &fakeDispatcher{}
	sink := &fakeSink{createNext: true}
	var landed []struct {
		taskID   string
		prNumber int
		state    string
	}
	svc := NewService(Deps{
		Cfg:                 cfg,
		Tasks:               tasks,
		Audit:               fakeAudit{},
		Agents:              nilAgentLister{},
		DowngradeLLMForTask: func(taskID string) bool { return taskID == "hr-merged" },
		FetchPRState: func(repo string, number int) (github.PRState, error) {
			if repo != "o/r" || number != 42 {
				t.Fatalf("FetchPRState(%q, %d)", repo, number)
			}
			return github.PRState{State: "MERGED"}, nil
		},
		LandClosedPR: func(_ context.Context, taskID string, prNumber int, state string) error {
			landed = append(landed, struct {
				taskID   string
				prNumber int
				state    string
			}{taskID: taskID, prNumber: prNumber, state: state})
			tasks.mu.Lock()
			defer tasks.mu.Unlock()
			for i := range tasks.tasks {
				if tasks.tasks[i].ID != taskID {
					continue
				}
				tasks.tasks[i].Status = task.StatusDone
				tasks.tasks[i].Outcome = "merged"
				tasks.tasks[i].Workflow = nil
				break
			}
			return nil
		},
		Dispatcher: disp,
		Sink:       sink,
		Logger:     slog.Default(),
		Now:        func() time.Time { return now },
	})

	report, err := svc.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(tasks.updates) != 0 {
		t.Fatalf("monitor must not update task directly, got %d updates", len(tasks.updates))
	}
	if len(landed) != 1 {
		t.Fatalf("want 1 landing callback, got %d", len(landed))
	}
	if landed[0].taskID != "hr-merged" || landed[0].prNumber != 42 || landed[0].state != "MERGED" {
		t.Fatalf("landing callback = %+v, want hr-merged #42 MERGED", landed[0])
	}
	if len(disp.calls) != 0 {
		t.Fatalf("dispatcher must not be called, got %d calls", len(disp.calls))
	}
	if len(sink.submissions) != 0 {
		t.Fatalf("sink must not see merged-pr anomaly, got %d submissions", len(sink.submissions))
	}
	if len(report.Remediated) != 1 || report.Remediated[0] != "linked_pr_merged:hr-merged" {
		t.Fatalf("remediated = %v, want pre-sweep merged close label", report.Remediated)
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
