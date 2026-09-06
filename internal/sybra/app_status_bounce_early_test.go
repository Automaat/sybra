package sybra

import (
	"sync/atomic"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestRegressionLateBoundaryCannotQuarantineNewEpisode(t *testing.T) {
	_, a := setupTaskService(t)
	tk, err := a.tasks.CreateWithStatus("Synthetic next incident", "", "headless", task.StatusInReview, task.Update{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	a.tasks.SetStatusTransitionObserver(a.observeIncidentBoundary)
	var tripped atomic.Bool
	a.tasks.SetStatusTransitionHook(func(id, from, to, actor string, snapshot task.Task) {
		if actor == "monitor.incident.reopen" {
			close(entered)
			<-release
		}
		if a.statusBounceTrippedAt(id, from, to, actor, snapshot.UpdatedAt) {
			tripped.Store(true)
		}
	})
	apply := func(status task.Status, actor string) {
		t.Helper()
		if _, err := a.tasks.Apply(task.TransitionIntent{TaskID: tk.ID, ToStatus: status, Actor: actor, OperatorOverride: true}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		apply(task.StatusInProgress, "fixer")
		apply(task.StatusInReview, "reviewer")
	}
	apply(task.StatusDone, "completed")
	go func() {
		_, err := a.tasks.Apply(task.TransitionIntent{TaskID: tk.ID, ToStatus: task.StatusTodo, Actor: "monitor.incident.reopen", OperatorOverride: true})
		done <- err
	}()
	<-entered
	apply(task.StatusInReview, "initial")
	apply(task.StatusInProgress, "fixer")
	falsePositive := tripped.Load()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if falsePositive {
		t.Fatal("first edge in new incident episode triggered quarantine using prior episode counts before delayed boundary arrived")
	}
}
