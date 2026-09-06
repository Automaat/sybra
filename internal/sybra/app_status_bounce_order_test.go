package sybra

import (
	"sync/atomic"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestRegressionLateIncidentDoesNotEraseLiveEpisode(t *testing.T) {
	_, a := setupTaskService(t)
	tk, err := a.tasks.CreateWithStatus("Synthetic incident ordering", "", "headless", task.StatusDone, task.Update{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var tripped atomic.Bool
	a.tasks.SetStatusTransitionObserver(a.observeIncidentBoundary)
	a.tasks.SetStatusTransitionHook(func(id, from, to, actor string, snapshot task.Task) {
		if actor == "monitor.incident.reopen" {
			close(entered)
			<-release
		}
		if a.statusBounceTrippedAt(id, from, to, actor, snapshot.UpdatedAt) {
			tripped.Store(true)
		}
	})
	go func() {
		_, err := a.tasks.Apply(task.TransitionIntent{TaskID: tk.ID, ToStatus: task.StatusTodo, Actor: "monitor.incident.reopen", OperatorOverride: true})
		done <- err
	}()
	<-entered
	apply := func(status task.Status, actor string) {
		t.Helper()
		if _, err := a.tasks.Apply(task.TransitionIntent{TaskID: tk.ID, ToStatus: status, Actor: actor, OperatorOverride: true}); err != nil {
			t.Fatal(err)
		}
	}
	apply(task.StatusInReview, "initial")
	for range 2 {
		apply(task.StatusInProgress, "fixer")
		apply(task.StatusInReview, "reviewer")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tripped.Load() {
		t.Fatal("unexpected early trip")
	}
	apply(task.StatusInProgress, "fixer")
	if !tripped.Load() {
		t.Fatal("late incident hook erased the 4 newer episode edges; 3-forward/2-reverse contention escaped")
	}
}
