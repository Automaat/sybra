package audit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

var factoryEpoch = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
var factoryRevision = strings.Repeat("a", 40)

func factoryEvent(second int, kind, taskID, agentID string, data map[string]any) Event {
	return Event{Timestamp: factoryEpoch.Add(time.Duration(second) * time.Second), Type: kind, TaskID: taskID, AgentID: agentID, Data: data, Release: factoryRevision}
}

func factoryQuery() FactoryQuery {
	return FactoryQuery{Since: factoryEpoch, Until: factoryEpoch.Add(time.Hour)}
}

func TestFactoryCanonicalRunsAndUniqueCompletions(t *testing.T) {
	events := []Event{
		factoryEvent(1, EventAgentStarted, "private-synthetic-task", "one", map[string]any{"role": "review"}),
		factoryEvent(2, EventAgentStarted, "private-synthetic-task", "one", map[string]any{"role": "review"}), // resume, not new work
		factoryEvent(21, EventAgentCompleted, "private-synthetic-task", "one", map[string]any{"role": "review", "state": "failed", "cost_usd": float64(2)}),
		factoryEvent(21, EventAgentFailed, "private-synthetic-task", "one", map[string]any{"role": "review", "cost_usd": float64(2)}),
		factoryEvent(30, EventAgentStarted, "private-synthetic-task", "two", map[string]any{"role": "review"}),
		factoryEvent(70, EventAgentCompleted, "private-synthetic-task", "two", map[string]any{"role": "review", "state": "stopped", "cost_usd": float64(3)}),
		factoryEvent(80, EventTaskStatusChanged, "private-synthetic-task", "", map[string]any{"from": string(taskstatus.InReview), "to": string(taskstatus.Done), "title": "PRIVATE CONTENT"}),
		factoryEvent(90, EventTaskStatusChanged, "private-synthetic-task", "", map[string]any{"from": string(taskstatus.Done), "to": string(taskstatus.Todo)}),
		factoryEvent(100, EventTaskStatusChanged, "private-synthetic-task", "", map[string]any{"from": string(taskstatus.Todo), "to": string(taskstatus.Done)}),
	}
	events = append(events, events[6])
	slices.Reverse(events)
	r, err := SummarizeFactory(events, factoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	if r.AgentRuns != 2 || r.RetriesAfterFailure != 1 || r.UniqueCompletedTasks != 1 || r.ReopenedTasks != 1 || r.ObservedCostUSD != 5 || r.CompletedTaskWindowCostUSD != 5 {
		t.Fatalf("accounting: %+v", r)
	}
	p := r.Phases["agent"]
	if p.Samples != 2 || *p.MedianSeconds != 30 || *p.P95Seconds != 40 {
		t.Fatalf("latency: %+v", p)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-synthetic-task", "PRIVATE CONTENT", "agent_id", "task_id"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("aggregate leaked %q", secret)
		}
	}
}

func TestFactoryObservedPhaseBoundaries(t *testing.T) {
	events := []Event{
		factoryEvent(1, EventFactoryQueue, "t", "", map[string]any{"interval_key": "q", "state": "queued"}),
		factoryEvent(16, EventFactoryQueue, "t", "", map[string]any{"interval_key": "q", "state": "dequeued"}),
		factoryEvent(20, EventFactoryCIWait, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(30, EventFactoryCIWait, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(50, EventFactoryCIVerified, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(55, EventFactoryCIVerified, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(10, EventAutoUpdateTransition, "", "", map[string]any{"transition": "seen", "candidate": factoryRevision}),
		factoryEvent(60, EventFactoryReleaseStarted, "", "", nil),
		factoryEvent(70, EventFactoryReleaseStarted, "", "", nil),
	}
	r, err := SummarizeFactory(events, factoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	for phase, want := range map[string]float64{"queue": 15, "ci": 30, "deploy": 50} {
		p := r.Phases[phase]
		if p.Samples != 1 || p.MedianSeconds == nil || *p.MedianSeconds != want {
			t.Errorf("%s: %+v, want %v seconds", phase, p, want)
		}
	}
}

func TestFactoryPartialWindowsAndQueueRestore(t *testing.T) {
	events := []Event{
		factoryEvent(-10, EventAgentStarted, "t", "old", nil),
		factoryEvent(10, EventAgentCompleted, "t", "old", map[string]any{"state": "stopped"}),
		factoryEvent(20, EventAgentStarted, "t", "open", nil),
		factoryEvent(1, EventFactoryQueue, "t", "", map[string]any{"interval_key": "restored", "state": "queued"}),
		factoryEvent(2, EventFactoryQueue, "t", "", map[string]any{"interval_key": "restored", "state": "dequeued"}),
		factoryEvent(3, EventFactoryQueue, "t", "", map[string]any{"interval_key": "restored", "state": "queued"}),
		factoryEvent(4, EventFactoryQueue, "t", "", map[string]any{"interval_key": "dropped", "state": "queued"}),
		factoryEvent(5, EventFactoryQueue, "t", "", map[string]any{"interval_key": "dropped", "state": "removed"}),
		factoryEvent(3600, EventAgentCompleted, "t", "open", map[string]any{"state": "stopped"}), // exclusive end
	}
	r, err := SummarizeFactory(events, factoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Phases["agent"]; p.Open != 1 || p.Unknown != 1 || p.Samples != 0 || p.P95Seconds != nil {
		t.Fatalf("invented agent samples: %+v", p)
	}
	if p := r.Phases["queue"]; p.Open != 1 || p.Censored != 1 || p.Samples != 0 {
		t.Fatalf("queue restore: %+v", p)
	}
	if r.UnknownCostRuns != 1 {
		t.Fatalf("missing cost treated as free: %+v", r)
	}
}

func TestFactoryReleaseFilteringDoesNotRelabelHistory(t *testing.T) {
	events := []Event{factoryEvent(1, EventAgentStarted, "t", "one", nil), factoryEvent(10, EventAgentCompleted, "t", "one", map[string]any{"state": "stopped"})}
	events[0].Release = ""
	for _, filter := range []string{factoryRevision, "unknown", "mixed"} {
		q := factoryQuery()
		q.Release = filter
		r, err := SummarizeFactory(events, q)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if filter == "unknown" {
			want = 1
		}
		if r.AgentRuns != want {
			t.Fatalf("%s runs=%d, want %d", filter, r.AgentRuns, want)
		}
	}
	events[0].Release = strings.Repeat("b", 40)
	q := factoryQuery()
	q.Release = "mixed"
	r, err := SummarizeFactory(events, q)
	if err != nil || r.AgentRuns != 1 {
		t.Fatalf("mixed lost: %+v %v", r, err)
	}
}

func TestFactoryZeroAndBounds(t *testing.T) {
	r, err := SummarizeFactory(nil, factoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	for phase, p := range r.Phases {
		if !p.Unavailable || p.MedianSeconds != nil || p.P95Seconds != nil {
			t.Fatalf("zero %s: %+v", phase, p)
		}
	}
	q := factoryQuery()
	q.Until = q.Since.Add(FactoryMaxWindow + time.Second)
	if _, err := SummarizeFactory(nil, q); err == nil {
		t.Fatal("unbounded query accepted")
	}
	q = factoryQuery()
	q.Release = "private-branch-name"
	if _, err := SummarizeFactory(nil, q); err == nil {
		t.Fatal("arbitrary release accepted")
	}
	over := make([]Event, FactoryMaxEvents+1)
	for i := range over {
		over[i] = factoryEvent(1, "synthetic", "", "", nil)
	}
	if _, err := SummarizeFactory(over, factoryQuery()); err == nil {
		t.Fatal("unbounded event set accepted")
	}
	for i := range over {
		over[i].Timestamp = factoryQuery().Until
	}
	if _, err := SummarizeFactory(over, factoryQuery()); err != nil {
		t.Fatalf("exclusive-end events counted: %v", err)
	}
}

func TestFactoryCIRerunOnSameHeadIsNewWait(t *testing.T) {
	events := []Event{
		factoryEvent(1, EventFactoryCIWait, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(11, EventFactoryCIVerified, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(100, EventFactoryCIWait, "t", "", map[string]any{"interval_key": "ci"}),
		factoryEvent(120, EventFactoryCIVerified, "t", "", map[string]any{"interval_key": "ci"}),
	}
	r, err := SummarizeFactory(events, factoryQuery())
	if err != nil {
		t.Fatal(err)
	}
	p := r.Phases["ci"]
	if p.Samples != 2 || *p.MedianSeconds != 15 {
		t.Fatalf("rerun lost: %+v", p)
	}
}

func TestFactoryRedeploySameRevisionStartsNewEpisode(t *testing.T) {
	events := []Event{
		factoryEvent(1, EventFactoryReleaseStarted, "", "", nil),
		factoryEvent(20, EventAutoUpdateTransition, "", "", map[string]any{"transition": "seen", "candidate": factoryRevision}),
		factoryEvent(30, EventFactoryReleaseStarted, "", "", nil),
	}
	events[1].Release = strings.Repeat("b", 40)
	q := factoryQuery()
	q.Release = factoryRevision
	r, err := SummarizeFactory(events, q)
	if err != nil {
		t.Fatal(err)
	}
	p := r.Phases["deploy"]
	if p.Samples != 1 || p.Unknown != 1 || p.MedianSeconds == nil || *p.MedianSeconds != 10 {
		t.Fatalf("redeploy lost: %+v", p)
	}
}

func TestFactoryOverlappingRunUsesTerminalOrderForRetries(t *testing.T) {
	events := []Event{
		factoryEvent(1, EventAgentStarted, "t", "long-failure", nil),
		factoryEvent(10, EventAgentStarted, "t", "short-success", nil),
		factoryEvent(20, EventAgentCompleted, "t", "short-success", map[string]any{"state": "stopped"}),
		factoryEvent(30, EventAgentCompleted, "t", "long-failure", map[string]any{"state": "failed"}),
		factoryEvent(40, EventAgentStarted, "t", "retry", nil),
	}
	r, err := SummarizeFactory(events, factoryQuery())
	if err != nil || r.RetriesAfterFailure != 1 {
		t.Fatalf("terminal-order retry lost: %+v %v", r, err)
	}
}

func TestFactoryReleaseDecoratorPreservesHistoricalRead(t *testing.T) {
	store, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := factoryEvent(1, EventAgentStarted, "t", "old", nil)
	old.Release = ""
	if err := store.Log(old); err != nil {
		t.Fatal(err)
	}
	live := WithRelease(store, factoryRevision)
	if err := live.Log(factoryEvent(2, EventAgentStarted, "t", "fresh-run", nil)); err != nil {
		t.Fatal(err)
	}
	got, err := live.Read(Query{Since: factoryEpoch, Until: factoryEpoch.Add(time.Hour)})
	if err != nil || len(got) != 2 || got[0].Release != "" || got[1].Release != factoryRevision {
		t.Fatalf("history relabeled: %+v %v", got, err)
	}
}
