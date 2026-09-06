package task

import "testing"

func TestStatusTransitionHookActorSurvivesConcurrentWrite(t *testing.T) {
	m, _ := newTestManager(t)
	tk, err := m.Create("Synthetic actor isolation", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	captured := make(chan string, 2)
	m.SetStatusTransitionHook(func(_, _, to, actor string, snapshot Task) {
		if to == string(StatusInProgress) {
			close(entered)
			<-release
		}
		captured <- actor + ":" + string(snapshot.Status)
	})
	firstDone := make(chan error, 1)
	go func() {
		_, err := m.Apply(TransitionIntent{TaskID: tk.ID, ToStatus: StatusInProgress, Actor: "first", OperatorOverride: true})
		firstDone <- err
	}()
	<-entered
	_, secondErr := m.Apply(TransitionIntent{TaskID: tk.ID, ToStatus: StatusBlocked, Actor: "second", OperatorOverride: true})
	close(release)
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := <-captured; got != "second:blocked" {
		t.Fatalf("second = %q", got)
	}
	if got := <-captured; got != "first:in-progress" {
		t.Fatalf("first = %q; concurrent actor leaked into earlier snapshot", got)
	}
}
