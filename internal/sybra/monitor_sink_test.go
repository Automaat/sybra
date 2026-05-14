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
	mu    sync.Mutex
	calls int
}

func (f *fakeInnerSink) Submit(_ context.Context, _ monitor.Anomaly, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return true, nil
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
