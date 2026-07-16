package workflow

import (
	"errors"
	"testing"
)

func TestSetAutoDispatch_GatesEveryEntryPoint(t *testing.T) {
	entries := []struct {
		name string
		call func(e *Engine) error
	}{
		{name: "StartWorkflow", call: func(e *Engine) error { return e.StartWorkflow("t1", "test-simple") }},
		{name: "StartWorkflowWithVars", call: func(e *Engine) error {
			return e.StartWorkflowWithVars("t1", "test-simple", nil)
		}},
		{name: "StartWorkflowFromStepWithVars", call: func(e *Engine) error {
			return e.StartWorkflowFromStepWithVars("t1", "test-simple", "", nil)
		}},
		{name: "DispatchEvent", call: func(e *Engine) error {
			_, err := e.DispatchEvent("t1", "task.created", nil, nil)
			return err
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name+"/disabled", func(t *testing.T) {
			e, agents := newAutoDispatchEngine(t)
			e.SetAutoDispatch(false)

			err := entry.call(e)

			if !errors.Is(err, ErrAutoDispatchDisabled) {
				t.Errorf("%s error = %v, want ErrAutoDispatchDisabled", entry.name, err)
			}
			if got := agents.LastCall().Role; got != "" {
				t.Errorf("%s started an agent (role %q) with dispatch disabled", entry.name, got)
			}
		})
		t.Run(entry.name+"/enabled", func(t *testing.T) {
			e, _ := newAutoDispatchEngine(t)
			e.SetAutoDispatch(true)

			if err := entry.call(e); errors.Is(err, ErrAutoDispatchDisabled) {
				t.Errorf("%s returned ErrAutoDispatchDisabled while enabled", entry.name)
			}
		})
	}
}

func TestSetAutoDispatch_HandleStatusChangeAndResumeStalled(t *testing.T) {
	tests := []struct {
		name string
		call func(e *Engine)
	}{
		{name: "HandleStatusChange", call: func(e *Engine) { e.HandleStatusChange("t1", "in-progress") }},
		{name: "ResumeStalled", call: func(e *Engine) { e.ResumeStalled() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, agents := newAutoDispatchEngine(t)
			e.SetAutoDispatch(false)

			tt.call(e)

			if got := agents.LastCall().Role; got != "" {
				t.Errorf("%s started an agent (role %q) with dispatch disabled", tt.name, got)
			}
		})
	}
}

func TestEngineZeroValueDispatches(t *testing.T) {
	e, _ := newAutoDispatchEngine(t)

	if err := e.StartWorkflow("t1", "test-simple"); errors.Is(err, ErrAutoDispatchDisabled) {
		t.Fatal("a freshly constructed Engine refused to dispatch; the zero value must keep pre-gate behavior")
	}
}

func newAutoDispatchEngine(t *testing.T) (*Engine, *mockAgents) {
	t.Helper()
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	return NewEngine(store, tasks, agents, discardLogger()), agents
}
