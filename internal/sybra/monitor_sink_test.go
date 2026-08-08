package sybra

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/monitor"
	"github.com/Automaat/sybra/internal/task"
)

type fakeInnerSink struct {
	mu         sync.Mutex
	calls      int
	closeCalls int
	closeNext  bool
}

func (f *fakeInnerSink) Submit(_ context.Context, _ monitor.Anomaly, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return true, nil
}

func (f *fakeInnerSink) CloseIfOpen(_ context.Context, _ monitor.Anomaly, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return f.closeNext, nil
}

func newSinkTestEnv(t *testing.T) (*monitorRoutingSink, *task.Manager, *fakeInnerSink) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	inner := &fakeInnerSink{}
	workCtx := func(projectID string) *WorkScrubContext {
		if projectID == "" {
			return nil
		}
		return &WorkScrubContext{ProjectID: projectID, Blocklist: []string{"kumahq/kuma"}}
	}
	sink := newMonitorRoutingSink(inner, tasks, workCtx, "Automaat/sybra", nil, slog.New(slog.DiscardHandler))
	sink.now = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return sink, tasks, inner
}

func TestMonitorRoutingSink_WorkAnomaly_CreatesLocalTaskWithProject(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	created, err := sink.Submit(context.Background(), a, "evidence with kumahq/kuma leak")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first call")
	}
	if inner.calls != 0 {
		t.Fatalf("inner sink should not be called for work anomaly, got calls=%d", inner.calls)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var routed *task.Task
	for i := range all {
		if strings.HasPrefix(all[i].Title, "[monitor] stuck_human_blocked") {
			routed = &all[i]
			break
		}
	}
	if routed == nil {
		t.Fatalf("no routed task created; tasks=%+v", all)
		return
	}
	if routed.ProjectID != "Automaat/sybra" {
		t.Errorf("ProjectID = %q, want %q", routed.ProjectID, "Automaat/sybra")
	}
	if !slices.Contains(routed.Tags, "sybra-bug") || !slices.Contains(routed.Tags, "scrubbed") ||
		!slices.Contains(routed.Tags, "monitor:stuck_human_blocked") {
		t.Errorf("tags missing required values: %v", routed.Tags)
	}
	if strings.Contains(routed.Body, "kumahq/kuma") {
		t.Errorf("body leaked work identifier: %q", routed.Body)
	}
}

func TestMonitorRoutingSink_WorkAnomaly_DispatchesCreatedWorkflow(t *testing.T) {
	t.Parallel()
	sink, tasks, _ := newSinkTestEnv(t)
	var dispatched []string
	sink.dispatchCreated = func(taskID string) {
		dispatched = append(dispatched, taskID)
	}
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	if _, err := sink.Submit(context.Background(), a, "evidence"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(dispatched) != 1 {
		t.Fatalf("dispatch calls = %d, want 1", len(dispatched))
	}
	routed, err := tasks.Get(dispatched[0])
	if err != nil {
		t.Fatalf("Get dispatched task: %v", err)
	}
	if routed.ProjectID != "Automaat/sybra" {
		t.Fatalf("dispatch happened before project route update: project_id=%q", routed.ProjectID)
	}
	if !slices.Contains(routed.Tags, "sybra-bug") {
		t.Fatalf("dispatch happened before tag route update: tags=%v", routed.Tags)
	}
}

func TestMonitorRoutingSink_WorkAnomaly_DedupsByTitle(t *testing.T) {
	t.Parallel()
	sink, tasks, _ := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	if _, err := sink.Submit(context.Background(), a, "first evidence"); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	created, err := sink.Submit(context.Background(), a, "second evidence")
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on dedup hit")
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var routed []task.Task
	for _, tk := range all {
		if strings.HasPrefix(tk.Title, "[monitor] stuck_human_blocked") {
			routed = append(routed, tk)
		}
	}
	if len(routed) != 1 {
		t.Fatalf("want 1 routed task after dedup, got %d", len(routed))
	}
	if !strings.Contains(routed[0].Body, "## Re-detected at 2026-05-11T12:00:00Z") {
		t.Errorf("body missing re-detected note: %q", routed[0].Body)
	}
	if !strings.Contains(routed[0].Body, "second evidence") {
		t.Errorf("body missing appended evidence: %q", routed[0].Body)
	}
}

func TestMonitorRoutingSink_ConfidentialIncidentResolvesAndReopensSameLocalTask(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	pid := sink.workCtx("fixture").Blocklist[0]
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatal(err)
	}
	in := monitor.Incident{Fingerprint: "incident:opaque", FailureCode: string(monitor.KindLostAgent), ProjectScope: "work-opaque", Revision: 1, State: monitor.IncidentActive}
	if created, _, err := sink.ApplyIncident(context.Background(), in, monitor.IncidentOpened, "- Fingerprint: `"+in.Fingerprint+"`\n"+pid+" evidence"); err != nil || !created {
		t.Fatalf("open: created=%v err=%v", created, err)
	}
	if closed, err := sink.ResolveIncident(context.Background(), monitor.Incident{Fingerprint: in.Fingerprint, FailureCode: in.FailureCode, ProjectScope: in.ProjectScope, Revision: 2, State: monitor.IncidentResolved}, "resolved"); err != nil || !closed {
		t.Fatalf("resolve: closed=%v err=%v", closed, err)
	}
	in.Revision = 3
	if created, _, err := sink.ApplyIncident(context.Background(), in, monitor.IncidentReopened, pid+" recurred"); err != nil || created {
		t.Fatalf("reopen: created=%v err=%v", created, err)
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	var incidentTasks []task.Task
	for _, candidate := range all {
		if strings.Contains(candidate.Body, in.Fingerprint) {
			incidentTasks = append(incidentTasks, candidate)
		}
	}
	if len(incidentTasks) != 1 || incidentTasks[0].Status != task.StatusTodo {
		t.Fatalf("incident history was not reopened in place: %+v", incidentTasks)
	}
	if strings.Contains(incidentTasks[0].Body, pid) {
		t.Fatalf("local incident body was not scrubbed: %q", incidentTasks[0].Body)
	}
	if inner.calls != 0 || inner.closeCalls != 0 {
		t.Fatalf("confidential incident reached public sink: submit=%d close=%d", inner.calls, inner.closeCalls)
	}
}

func TestMonitorRoutingSink_LegacyInnerDoesNotInventIncidentURL(t *testing.T) {
	t.Parallel()
	sink, _, inner := newSinkTestEnv(t)
	in := monitor.Incident{
		Fingerprint:  "incident:opaque",
		FailureCode:  string(monitor.KindLostAgent),
		ProjectScope: "fleet",
		State:        monitor.IncidentActive,
	}
	created, artifact, err := sink.ApplyIncident(context.Background(), in, monitor.IncidentOpened, "evidence")
	if err != nil || !created {
		t.Fatalf("ApplyIncident: created=%v err=%v", created, err)
	}
	if artifact.URL != "" {
		t.Fatalf("legacy inner invented durable incident URL %q", artifact.URL)
	}
	if inner.calls != 1 {
		t.Fatalf("legacy inner submissions = %d, want 1", inner.calls)
	}
}

func TestMonitorRoutingSink_ConfidentialRoutesFailClosedWithoutWorkScrubContext(t *testing.T) {
	t.Parallel()
	inner := &fakeInnerSink{closeNext: true}
	sink := newMonitorRoutingSink(inner, nil, nil, "Automaat/sybra", nil, slog.New(slog.DiscardHandler))
	a := monitor.Anomaly{
		Kind:         monitor.KindLostAgent,
		Fingerprint:  "incident:opaque",
		Confidential: true,
	}

	if _, err := sink.Submit(context.Background(), a, "sensitive evidence"); err == nil {
		t.Fatal("Submit error = nil, want fail-closed error")
	}
	incident := monitor.Incident{
		Fingerprint:  a.Fingerprint,
		FailureCode:  string(a.Kind),
		ProjectScope: "work-opaque",
		State:        monitor.IncidentActive,
	}
	if _, _, err := sink.ApplyIncident(context.Background(), incident, monitor.IncidentOpened, "sensitive evidence"); err == nil {
		t.Fatal("ApplyIncident error = nil, want fail-closed error")
	}
	if _, err := sink.CloseIfOpen(context.Background(), a, "sensitive evidence"); err == nil {
		t.Fatal("CloseIfOpen error = nil, want fail-closed error")
	}
	if inner.calls != 0 || inner.closeCalls != 0 {
		t.Fatalf("confidential route reached public sink: submit=%d close=%d", inner.calls, inner.closeCalls)
	}
}

func TestMonitorRoutingSink_WorkRoutesFailClosedWithIneffectiveScrubContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	inner := &fakeInnerSink{closeNext: true}
	sink := newMonitorRoutingSink(inner, tasks, func(string) *WorkScrubContext {
		return &WorkScrubContext{ProjectID: "work-project", Blocklist: []string{"", " \t "}}
	}, "Automaat/sybra", nil, slog.New(slog.DiscardHandler))
	src, err := tasks.Create("source", "source body", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	pid := "work-project"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatal(err)
	}
	a := monitor.Anomaly{
		Kind:         monitor.KindLostAgent,
		TaskID:       src.ID,
		Fingerprint:  "lost_agent:" + src.ID,
		Confidential: true,
	}

	if _, err := sink.Submit(context.Background(), a, "SECRET submit"); err == nil {
		t.Fatal("Submit error = nil, want fail-closed error")
	}
	if _, err := sink.CloseIfOpen(context.Background(), a, "SECRET close"); err == nil {
		t.Fatal("CloseIfOpen error = nil, want fail-closed error")
	}
	incident := monitor.Incident{
		Fingerprint:  "incident:opaque",
		FailureCode:  string(a.Kind),
		ProjectScope: "work-opaque",
		State:        monitor.IncidentActive,
	}
	if _, _, err := sink.ApplyIncident(context.Background(), incident, monitor.IncidentOpened, "SECRET incident"); err == nil {
		t.Fatal("ApplyIncident error = nil, want fail-closed error")
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Body != "source body" || all[0].Status != task.StatusTodo {
		t.Fatalf("confidential route mutated local tasks: %+v", all)
	}
	if inner.calls != 0 || inner.closeCalls != 0 {
		t.Fatalf("confidential route reached public sink: submit=%d close=%d", inner.calls, inner.closeCalls)
	}
}

func TestMonitorRoutingSink_WorkAnomaly_DedupsByFingerprintAfterRename(t *testing.T) {
	t.Parallel()
	sink, tasks, _ := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	// First submit creates the chore task.
	body := "## Detection\n- Fingerprint: `stuck_human_blocked:" + src.ID + "`\n\n## Affected task\n- `" + src.ID + "`\n"
	if _, err := sink.Submit(context.Background(), a, body); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	// Simulate triage renaming the task (title changes, tags cleared).
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var chore *task.Task
	for i := range all {
		if strings.HasPrefix(all[i].Title, "[monitor]") {
			chore = &all[i]
			break
		}
	}
	if chore == nil {
		t.Fatalf("chore task not created")
		return
	}
	renamed := "chore(sybra): unblock task " + src.ID + " stuck in human-required"
	if _, err := tasks.Update(chore.ID, task.Update{Title: &renamed}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Second submit must dedup via fingerprint in body, not title.
	created, err := sink.Submit(context.Background(), a, "new cycle evidence")
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on fingerprint-dedup hit after rename")
	}
	all, err = tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var open int
	for _, tk := range all {
		if strings.Contains(tk.Title, src.ID) && !task.IsTerminalStatus(tk.Status) {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("want 1 open chore task after dedup, got %d", open)
	}
}

func TestMonitorRoutingSink_TerminalTaskReopensNew(t *testing.T) {
	t.Parallel()
	sink, tasks, _ := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	if _, err := sink.Submit(context.Background(), a, "first"); err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	all, _ := tasks.List()
	for _, tk := range all {
		if strings.HasPrefix(tk.Title, "[monitor] stuck_human_blocked") {
			done := task.StatusDone
			if _, err := tasks.Update(tk.ID, task.Update{Status: &done}); err != nil {
				t.Fatalf("close task: %v", err)
			}
		}
	}

	created, err := sink.Submit(context.Background(), a, "second")
	if err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true after prior closed")
	}
	all, _ = tasks.List()
	var open int
	for _, tk := range all {
		if strings.HasPrefix(tk.Title, "[monitor] stuck_human_blocked") && !task.IsTerminalStatus(tk.Status) {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("want exactly 1 open routed task, got %d", open)
	}
}

func TestMonitorRoutingSink_CloseIfOpen_WorkAnomalyMarksLocalTaskDone(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindLostAgent,
		TaskID:      src.ID,
		Fingerprint: "lost_agent:" + src.ID,
	}
	if _, err := sink.Submit(context.Background(), a, "first evidence"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	closed, err := sink.CloseIfOpen(context.Background(), a, "monitor: condition cleared")
	if err != nil {
		t.Fatalf("CloseIfOpen: %v", err)
	}
	if !closed {
		t.Fatal("want closed=true for a matching open local task")
	}
	if inner.closeCalls != 0 {
		t.Fatalf("inner sink should not be closed for a work anomaly, got %d calls", inner.closeCalls)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var routed *task.Task
	for i := range all {
		if strings.HasPrefix(all[i].Title, "[monitor] lost_agent") {
			routed = &all[i]
			break
		}
	}
	if routed == nil {
		t.Fatalf("routed task not found; tasks=%+v", all)
		return
	}
	if routed.Status != task.StatusDone {
		t.Errorf("status = %q, want done", routed.Status)
	}
	if strings.Contains(routed.Body, "kumahq/kuma") {
		t.Errorf("close comment leaked work identifier: %q", routed.Body)
	}
}

func TestMonitorRoutingSink_CloseIfOpen_WorkAnomalyClosesLocalTaskAfterSourceDeleted(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	src, err := tasks.Create("source", "src body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pid := "kumahq/kuma"
	if _, err := tasks.Update(src.ID, task.Update{ProjectID: &pid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindUntriaged,
		TaskID:      src.ID,
		Fingerprint: monitor.Fingerprint(monitor.KindUntriaged, src.ID, nil),
	}
	if _, err := sink.Submit(context.Background(), a, monitor.DeterministicIssueBody(a)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var routedID string
	for i := range all {
		if strings.HasPrefix(all[i].Title, "[monitor] untriaged") {
			routedID = all[i].ID
			break
		}
	}
	if routedID == "" {
		t.Fatalf("routed task not found; tasks=%+v", all)
	}
	if err := tasks.Delete(src.ID); err != nil {
		t.Fatalf("delete source: %v", err)
	}

	closed, err := sink.CloseIfOpen(context.Background(), a, "monitor: condition cleared")
	if err != nil {
		t.Fatalf("CloseIfOpen: %v", err)
	}
	if !closed {
		t.Fatal("want closed=true for matching local task after source deletion")
	}
	if inner.closeCalls != 0 {
		t.Fatalf("inner sink should not be closed for a routed local task, got %d calls", inner.closeCalls)
	}

	routed, err := tasks.Get(routedID)
	if err != nil {
		t.Fatalf("Get routed: %v", err)
	}
	if routed.Status != task.StatusDone {
		t.Errorf("status = %q, want done", routed.Status)
	}
}

func TestMonitorRoutingSink_CloseIfOpen_NonWorkPassesThroughToInner(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	src, err := tasks.Create("source", "body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindLostAgent,
		TaskID:      src.ID,
		Fingerprint: "lost_agent:" + src.ID,
	}
	inner.closeNext = true
	closed, err := sink.CloseIfOpen(context.Background(), a, "monitor: condition cleared")
	if err != nil {
		t.Fatalf("CloseIfOpen: %v", err)
	}
	if !closed {
		t.Fatal("want the inner sink's close result surfaced")
	}
	if inner.closeCalls != 1 {
		t.Fatalf("inner.closeCalls = %d, want 1", inner.closeCalls)
	}
}

func TestMonitorRoutingSink_NonWorkPassesThrough(t *testing.T) {
	t.Parallel()
	sink, tasks, inner := newSinkTestEnv(t)
	// Source task with no project_id → workCtx returns nil → inner sink path.
	src, err := tasks.Create("source", "body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := monitor.Anomaly{
		Kind:        monitor.KindStuckHumanBlocked,
		TaskID:      src.ID,
		Fingerprint: "stuck_human_blocked:" + src.ID,
	}
	created, err := sink.Submit(context.Background(), a, "body")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatalf("inner returns created=true; got false")
	}
	if inner.calls != 1 {
		t.Fatalf("inner.calls = %d, want 1", inner.calls)
	}
}

func TestMonitorArtifactTitleKeepsIncidentIdentity(t *testing.T) {
	one := monitorArtifactTitle(monitor.Anomaly{Kind: monitor.KindLostAgent, Fingerprint: "incident:one"})
	two := monitorArtifactTitle(monitor.Anomaly{Kind: monitor.KindLostAgent, Fingerprint: "incident:two"})
	if one == two || !strings.Contains(one, "one") || !strings.Contains(two, "two") {
		t.Fatalf("incident titles collapsed: %q %q", one, two)
	}
}
