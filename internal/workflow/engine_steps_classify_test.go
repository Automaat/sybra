package workflow

import (
	"context"
	"errors"
	"testing"
)

func newClassifyTaskStep() *Step {
	return &Step{ID: "triage", Type: StepClassifyTask}
}

type fakeTaskClassifier struct {
	calls int
	taken string
	err   error
}

func (f *fakeTaskClassifier) ClassifyTask(_ context.Context, taskID string) error {
	f.calls++
	f.taken = taskID
	return f.err
}

func TestExecClassifyTask_Success(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	classifier := &fakeTaskClassifier{}
	engine.SetTaskClassifier(classifier)

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep())
	if err != nil {
		t.Fatalf("execClassifyTask: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	if classifier.calls != 1 || classifier.taken != "t1" {
		t.Errorf("classifier called with (%d, %q), want (1, t1)", classifier.calls, classifier.taken)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("task flipped to human-required on a successful classify")
	}
}

func TestExecClassifyTask_NilClassifier(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	// No SetTaskClassifier call — engine must flip to human-required.

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep())
	if err != nil {
		t.Fatalf("execClassifyTask: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}

func TestExecClassifyTask_ClassifierError(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetTaskClassifier(&fakeTaskClassifier{err: errors.New("provider unavailable")})

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep())
	if err != nil {
		t.Fatalf("execClassifyTask: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if ti.StatusReason == "" {
		t.Errorf("expected a status reason explaining the classifier failure")
	}
}
