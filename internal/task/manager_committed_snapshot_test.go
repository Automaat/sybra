package task

import "testing"

type regressionInterleavingPersistence struct {
	Persistence
	afterPut func()
}

func (p *regressionInterleavingPersistence) PutFnBy(id, actor string, fn func(Task) (Task, []string, error)) (Task, error) {
	saved, err := p.Persistence.PutFnBy(id, actor, fn)
	if err == nil && p.afterPut != nil {
		p.afterPut()
	}
	return saved, err
}

func TestRegressionAddRunActorBindsOwnCommittedSnapshot(t *testing.T) {
	second, _ := newTestManager(t)
	tk, err := second.Create("Synthetic concurrent snapshot", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	p := &regressionInterleavingPersistence{Persistence: second.persist}
	first := NewManagerWithPersistence(second.store, p, NoopEmitter())
	p.afterPut = func() {
		_, err := second.Apply(TransitionIntent{TaskID: tk.ID, ToStatus: StatusBlocked, Actor: "second", Extra: Update{
			Escalation: MachineFailure("synthetic.second", "second write"), AutonomyOutcome: QuarantinedOutcome(),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	first.SetStatusTransitionHook(func(_, from, to, actor string, snapshot Task) {
		if actor != "first" || from != string(StatusTodo) || to != string(StatusInProgress) || snapshot.Status != StatusInProgress || !snapshot.Escalation.IsZero() {
			t.Errorf("first writer's hook attributed another write: actor=%q from=%q to=%q status=%q escalation=%q", actor, from, to, snapshot.Status, snapshot.Escalation.Code)
		}
	})
	if err := first.AddRunWithStatusBy(tk.ID, "first", AgentRun{AgentID: "synthetic-first", State: "running"}, Ptr(StatusInProgress)); err != nil {
		t.Fatal(err)
	}
}
