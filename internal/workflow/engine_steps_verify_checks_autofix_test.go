package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func implementedExec() *Execution {
	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	now := time.Now().UTC()
	wf.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})
	return wf
}

func TestExecVerifyChecks_AutoFixRewindsToImplement(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"echo boom >&2; exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.StepID != "" || out.Status != "" {
		t.Fatalf("parked output should be zero, got %+v", out)
	}
	if wf.CurrentStep != verifyChecksImplStepID {
		t.Errorf("CurrentStep = %q, want %q", wf.CurrentStep, verifyChecksImplStepID)
	}
	if wf.State != ExecWaiting {
		t.Errorf("State = %q, want %q", wf.State, ExecWaiting)
	}
	if wf.Variables["step.verify_checks.auto_fix"] != "1" {
		t.Errorf("auto_fix counter = %q, want 1", wf.Variables["step.verify_checks.auto_fix"])
	}
	note := wf.Variables[verifyReaskNoteVar]
	if !strings.Contains(note, "FAILED the project verify suite") || !strings.Contains(note, "exit 1") {
		t.Errorf("reask note missing failure context:\n%s", note)
	}
	if wf.Variables[workflowRetryAfterVar] == "" {
		t.Errorf("retry-after not set")
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress (not escalated on first failure)", ti.Status)
	}
}

func TestExecVerifyChecks_AutoFixCapEscalates(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := implementedExec()
	wf.Variables["step.verify_checks.auto_fix"] = "2"
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required after cap", ti.Status)
	}
}

func TestExecVerifyChecks_NoImplementStepEscalates(t *testing.T) {
	t.Parallel()
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	engine, tasks := newVerifyChecksEngine(t, wt, []string{"exit 1"})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	wf := &Execution{Variables: map[string]string{}, StartedAt: time.Now().UTC()}
	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), wf, TaskInfo{ID: "t1", Status: "in-progress"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "flagged" {
		t.Errorf("Output = %q, want flagged (no implement step to rewind to)", out.Output)
	}
	if ti := mustGetTaskInfo(t, tasks, "t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
}
