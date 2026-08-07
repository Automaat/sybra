package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
)

func newAdmissionPreflightStep() *Step {
	return &Step{ID: "admission_preflight", Type: StepAdmissionPreflight}
}

func newAdmissionExec() *Execution {
	return &Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "admission_preflight",
		State:       ExecRunning,
		Variables:   map[string]string{},
	}
}

func TestExecAdmissionPreflight_DisabledIsNoOp(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	// admission zero-value: Enabled defaults false, matching every other
	// Engine dependency's nil-safe default.

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: `{"schema_version": "2"}`})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" || out.Output != "disabled" {
		t.Fatalf("out = %+v, want completed/disabled", out)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("disabled admission should never block: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecAdmissionPreflight_AdmitsNoContract(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc"})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" || out.Output != "admitted" {
		t.Fatalf("out = %+v, want completed/admitted", out)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("a noplan task must not be blocked: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecAdmissionPreflight_AdmitsValidContract(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" || out.Output != "admitted" {
		t.Fatalf("out = %+v, want completed/admitted", out)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Errorf("a valid contract must not be blocked: reason=%q", tasks.Reason("fa6919fc"))
	}
}

func TestExecAdmissionPreflight_InvalidContractBlocksAsOperatorDecision(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	// schema_version "2" with no objective — a missing machine-checkable
	// admission fact, covering the noplan/handoff path where
	// validate_plan_contract never ran during planning.
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"task_id": "fa6919fc",`,
		`"task_id": "fa6919fc",
  "schema_version": "2",`, 1)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" {
		t.Fatalf("out.Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.Blocker.Kind != blocker.KindOperatorDecision {
		t.Errorf("blocker kind = %q, want %q", ti.Blocker.Kind, blocker.KindOperatorDecision)
	}
	if !ti.Blocker.Exhausted {
		t.Error("blocker.Exhausted = false, want true (terminal, no re-attempts)")
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "objective is required") {
		t.Errorf("reason = %q, want missing objective", reason)
	}
}

func TestExecAdmissionPreflight_UnknownCapabilityBlocksAsOperatorDecision(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"task_id": "fa6919fc",`,
		`"task_id": "fa6919fc",
  "schema_version": "2",
  "objective": "ship the thing",
  "required_capabilities": ["launch_missiles"],`, 1)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" {
		t.Fatalf("out.Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.Blocker.Kind != blocker.KindOperatorDecision {
		t.Errorf("blocker kind = %q, want %q", ti.Blocker.Kind, blocker.KindOperatorDecision)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, `unknown capability "launch_missiles"`) {
		t.Errorf("reason = %q, want unknown capability", reason)
	}
}

func TestExecAdmissionPreflight_OversizeAcceptanceCriteriaBlocksAsOperatorDecision(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true, MaxAcceptanceCriteria: 1})
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"acceptance_criteria": ["implementation prompt includes the contract"]`,
		`"acceptance_criteria": ["first criterion", "second criterion"]`, 1)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" {
		t.Fatalf("out.Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.Blocker.Kind != blocker.KindOperatorDecision {
		t.Errorf("blocker kind = %q, want %q", ti.Blocker.Kind, blocker.KindOperatorDecision)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "acceptance_criteria count 2 exceeds configured limit 1") {
		t.Errorf("reason = %q, want oversize acceptance_criteria", reason)
	}
}

func TestExecAdmissionPreflight_OversizeFilesBlocksAsOperatorDecision(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true, MaxChangeSurfaceFiles: 1})
	contract := strings.Replace(validPlanContract("fa6919fc"),
		`"files": [
    {"path": "internal/workflow/engine.go", "purpose": "edit", "symbols": ["Engine"]}
  ],`,
		`"files": [
    {"path": "internal/workflow/engine.go", "purpose": "edit", "symbols": ["Engine"]},
    {"path": "internal/workflow/engine_advance.go", "purpose": "edit"}
  ],`, 1)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: contract})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" {
		t.Fatalf("out.Status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "files count 2 exceeds configured change-surface limit 1") {
		t.Errorf("reason = %q, want oversize files", reason)
	}
}

func TestExecAdmissionPreflight_NoWorktreeSkipsCredentialCheck(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})
	preflight := &fakePushPreflighter{err: errors.New("should never be called")}
	engine.setPushCredentialPreflighterForTest(preflight)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Output != "admitted" {
		t.Fatalf("out.Output = %q, want admitted (a not-yet-existing worktree is not a failure)", out.Output)
	}
	if preflight.calls != 0 {
		t.Errorf("preflight calls = %d, want 0", preflight.calls)
	}
}

func TestExecAdmissionPreflight_MissingCredentialsBlockAsCredentialRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: "/tmp/wt", ok: true})
	preflight := &fakePushPreflighter{err: errors.New("gh auth status: Bad credentials")}
	engine.setPushCredentialPreflighterForTest(preflight)

	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")})
	if err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if out.Status != "completed" {
		t.Fatalf("out.Status = %q, want completed", out.Status)
	}
	if preflight.calls != 1 {
		t.Fatalf("preflight calls = %d, want 1", preflight.calls)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.Blocker.Kind != blocker.KindCredentialRequired {
		t.Errorf("blocker kind = %q, want %q", ti.Blocker.Kind, blocker.KindCredentialRequired)
	}
	if !ti.Blocker.Exhausted {
		t.Error("blocker.Exhausted = false, want true (terminal, no re-attempts)")
	}
	if reason := tasks.Reason("fa6919fc"); !strings.Contains(reason, "Bad credentials") {
		t.Errorf("reason = %q, want push credential failure detail", reason)
	}
}

func TestExecAdmissionPreflight_TransientCredentialErrorParksForRetry(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: "/tmp/wt", ok: true})
	// A rate-limited/transient preflight hit must self-heal via a bounded
	// retry, not permanently strand a re-dispatched task at human-required.
	preflight := &fakePushPreflighter{err: errors.New("gh: API rate limit exceeded for GitHub")}
	engine.setPushCredentialPreflighterForTest(preflight)

	wfExec := newAdmissionExec()
	out, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), wfExec,
		TaskInfo{ID: "fa6919fc", Status: "in-progress", PlanContract: validPlanContract("fa6919fc")})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out.Status == "completed" {
		t.Fatalf("out = %+v, want the step parked (not completed)", out)
	}
	ti, _ := tasks.GetTask("fa6919fc")
	if ti.Status == "human-required" {
		t.Fatalf("status = %q, want the task parked in-progress (transient error must not strand it)", ti.Status)
	}
	if ti.Blocker.Exhausted {
		t.Error("blocker.Exhausted = true, want false (transient error is retriable)")
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("wfExec.State = %v, want ExecWaiting (parked for retry)", wfExec.State)
	}
}

// TestExecAdmissionPreflight_DisabledDecisionReasonDistinguishesSkip asserts
// that a disabled-admission no-op and a checks-ran-and-passed admission both
// report Outcome "admitted" but differ in Reason ("disabled" vs "admitted") —
// without this, the admission.decided audit event can't tell "checks were
// skipped" from "checks passed" (see PR #2631 review).
func TestExecAdmissionPreflight_DisabledDecisionReasonDistinguishesSkip(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	// admission zero-value: Enabled defaults false.
	var decisions []AdmissionDecision
	engine.SetAdmissionDecisionHook(func(_ TaskInfo, d AdmissionDecision) {
		decisions = append(decisions, d)
	})

	if _, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Outcome != "admitted" || decisions[0].Reason != "disabled" {
		t.Fatalf("decisions = %+v, want single admitted/disabled decision", decisions)
	}
}

func TestExecAdmissionPreflight_RecordsAdmissionDecision(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "fa6919fc", Status: "in-progress"})
	engine := newEngineForEval(t, tasks)
	engine.SetAdmissionConfig(config.AdmissionConfig{Enabled: true})
	var decisions []AdmissionDecision
	engine.SetAdmissionDecisionHook(func(_ TaskInfo, d AdmissionDecision) {
		decisions = append(decisions, d)
	})

	if _, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: validPlanContract("fa6919fc")}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %v, want exactly one", decisions)
	}
	if decisions[0].Outcome != "admitted" || decisions[0].RiskTier != "medium" || decisions[0].PermissionTier != "repo-write" {
		t.Fatalf("decision = %+v, want admitted/medium/repo-write", decisions[0])
	}
	if decisions[0].Reason != "admitted" {
		t.Fatalf("decision.Reason = %q, want %q — checks that actually ran must be distinguishable from a disabled no-op", decisions[0].Reason, "admitted")
	}

	invalid := strings.Replace(validPlanContract("fa6919fc"),
		`"task_id": "fa6919fc",`,
		`"task_id": "fa6919fc",
  "schema_version": "2",`, 1)
	if _, err := engine.execAdmissionPreflight("fa6919fc", newAdmissionPreflightStep(), newAdmissionExec(),
		TaskInfo{ID: "fa6919fc", PlanContract: invalid}); err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %v, want exactly two", decisions)
	}
	if decisions[1].Outcome != "blocked" || decisions[1].BlockerKind != string(blocker.KindOperatorDecision) {
		t.Fatalf("decision = %+v, want blocked/operator_decision", decisions[1])
	}
}
