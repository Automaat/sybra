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

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep(), &Execution{})
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

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep(), &Execution{})
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

// failNTimesClassifier errors on its first n calls, then succeeds — models a
// transient rate limit / brief provider outage the retry budget should absorb.
type failNTimesClassifier struct {
	failFirst int
	calls     int
}

func (f *failNTimesClassifier) ClassifyTask(_ context.Context, _ string) error {
	f.calls++
	if f.calls <= f.failFirst {
		return errors.New("provider unavailable")
	}
	return nil
}

func TestExecClassifyTask_RetriesTransientFailure(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	classifier := &failNTimesClassifier{failFirst: 2}
	engine.SetTaskClassifier(classifier)

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep(), &Execution{})
	if err != nil {
		t.Fatalf("execClassifyTask: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("status = %q, want completed", out.Status)
	}
	if classifier.calls != 3 {
		t.Errorf("classifier calls = %d, want 3 (2 transient failures + 1 success)", classifier.calls)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("task parked on human-required despite a retry succeeding")
	}
}

// ctxCancelClassifier always errors and records how many times it was invoked,
// so the test can assert the cancellation short-circuit skips the retry loop.
type ctxCancelClassifier struct{ calls int }

func (c *ctxCancelClassifier) ClassifyTask(_ context.Context, _ string) error {
	c.calls++
	return errors.New("provider unavailable")
}

func TestExecClassifyTask_ContextCanceledParksInsteadOfCompleting(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	classifier := &ctxCancelClassifier{}
	engine.SetTaskClassifier(classifier)

	// Engine shutdown mid-classify: the parent context is already canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	engine.SetContext(ctx)

	wfExec := &Execution{CurrentStep: "triage", State: ExecRunning}
	_, err := engine.execClassifyTask("t1", newClassifyTaskStep(), wfExec)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("execClassifyTask err = %v, want errStepParked", err)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("wfExec.State = %q, want %q", wfExec.State, ExecWaiting)
	}
	if wfExec.CurrentStep != "triage" {
		t.Errorf("wfExec.CurrentStep = %q, want unchanged %q", wfExec.CurrentStep, "triage")
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("engine shutdown baked human-required into the task instead of leaving it to resume")
	}
	if classifier.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 (cancellation must skip the retry loop)", classifier.calls)
	}
}

func TestExecClassifyTask_ClassifierError(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "new"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetTaskClassifier(&fakeTaskClassifier{err: errors.New("provider unavailable")})

	out, err := engine.execClassifyTask("t1", newClassifyTaskStep(), &Execution{})
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
