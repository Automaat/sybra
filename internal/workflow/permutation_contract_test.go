package workflow

import (
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
)

const (
	workflowPermutationTaskID     = "t1"
	workflowPermutationWorkflowID = "permutation-contract"
	workflowPermutationParentStep = "fan_out"
	workflowPermutationStatus     = "plan-review"
	workflowPermutationReason     = "parallel plans ready"
	workflowPermutationGateStep   = "review_gate"
)

type workflowPermutationScenario struct {
	t           *testing.T
	store       *Store
	tasks       *memTasks
	agents      *mockAgents
	engine      *Engine
	childRoutes map[string]workflowPermutationChildRoute
}

type workflowPermutationEvent struct {
	name  string
	apply func(*testing.T, *workflowPermutationScenario)
}

type workflowPermutationResult struct {
	EffectCount int
	Effects     []string
	State       workflowPermutationState
}

type workflowPermutationState struct {
	TaskStatus       string
	TaskStatusReason string
	WorkflowID       string
	CurrentStep      string
	ExecState        ExecState
	StepHistory      []string
	StepCounts       []string
	Variables        []string
	ParallelInflight []string
	AgentRoutes      []string
}

type workflowPermutationChildRoute struct {
	AgentID  string
	Provider string
}

func TestWorkflowPermutationContract(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	t.Cleanup(func() { providerAvailable = prev })

	baseline := runWorkflowPermutationContract(t,
		workflowPermutationComplete("child_a"),
		workflowPermutationComplete("child_b"),
	)
	assertWorkflowPermutationSettled(t, baseline)

	permutations := []struct {
		name   string
		events []workflowPermutationEvent
	}{
		{
			name: "reverse_completion_order",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_b"),
				workflowPermutationComplete("child_a"),
			},
		},
		{
			name: "duplicate_before_boundary_completes",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_a"),
				workflowPermutationDuplicate("child_a"),
				workflowPermutationComplete("child_b"),
			},
		},
		{
			name: "late_duplicate_after_parent_advance",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_a"),
				workflowPermutationComplete("child_b"),
				workflowPermutationDuplicate("child_b"),
			},
		},
		{
			name: "restart_after_first_child",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_a"),
				workflowPermutationRestart(),
				workflowPermutationComplete("child_b"),
			},
		},
		{
			name: "restart_after_first_child_reverse",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_b"),
				workflowPermutationRestart(),
				workflowPermutationComplete("child_a"),
			},
		},
		{
			name: "concurrent_status_reconcile_while_parallel_incomplete",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_a"),
				workflowPermutationConcurrentStatusChange(8),
				workflowPermutationComplete("child_b"),
			},
		},
		{
			name: "restart_then_status_reconcile_then_duplicate",
			events: []workflowPermutationEvent{
				workflowPermutationComplete("child_a"),
				workflowPermutationRestart(),
				workflowPermutationConcurrentStatusChange(8),
				workflowPermutationComplete("child_b"),
				workflowPermutationDuplicate("child_b"),
			},
		},
	}

	for _, tc := range permutations {
		t.Run(tc.name, func(t *testing.T) {
			got := runWorkflowPermutationContract(t, tc.events...)
			if !reflect.DeepEqual(got, baseline) {
				t.Fatalf("permutation result mismatch\n got: %#v\nwant: %#v", got, baseline)
			}
		})
	}
}

func runWorkflowPermutationContract(t *testing.T, events ...workflowPermutationEvent) workflowPermutationResult {
	t.Helper()

	scenario := newWorkflowPermutationScenario(t)
	for _, event := range events {
		event.apply(t, scenario)
	}

	return scenario.result()
}

func newWorkflowPermutationScenario(t *testing.T) *workflowPermutationScenario {
	t.Helper()

	store := newTestStore(t)
	if err := store.Save(workflowPermutationDefinition()); err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{
		ID:        workflowPermutationTaskID,
		Status:    "todo",
		AgentMode: "headless",
	})
	if err := engine.StartWorkflow(workflowPermutationTaskID, workflowPermutationWorkflowID); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	scenario := &workflowPermutationScenario{
		t:           t,
		store:       store,
		tasks:       tasks,
		agents:      agents,
		engine:      engine,
		childRoutes: make(map[string]workflowPermutationChildRoute),
	}
	scenario.assertParallelPending("initial start")
	return scenario
}

func workflowPermutationDefinition() Definition {
	return Definition{
		ID:   workflowPermutationWorkflowID,
		Name: "permutation contract",
		Trigger: Trigger{
			On: "manual",
		},
		Steps: []Step{
			{
				ID:   workflowPermutationParentStep,
				Name: "Fan Out",
				Type: StepParallel,
				Parallel: []Step{
					{
						ID:   "child_a",
						Name: "Child A",
						Type: StepRunAgent,
						Config: StepConfig{
							Role:     "child_a",
							Mode:     "headless",
							Provider: "claude",
							Prompt:   "plan a",
						},
					},
					{
						ID:   "child_b",
						Name: "Child B",
						Type: StepRunAgent,
						Config: StepConfig{
							Role:     "child_b",
							Mode:     "headless",
							Provider: "codex",
							Prompt:   "plan b",
						},
					},
				},
				Next: []Transition{{GoTo: "set_review"}},
			},
			{
				ID:   "set_review",
				Name: "Set Review Status",
				Type: StepSetStatus,
				Config: StepConfig{
					Status:       workflowPermutationStatus,
					StatusReason: workflowPermutationReason,
				},
				Next: []Transition{{GoTo: workflowPermutationGateStep}},
			},
			{
				ID:   workflowPermutationGateStep,
				Name: "Review Gate",
				Type: StepWaitHuman,
				Config: StepConfig{
					Status:       workflowPermutationStatus,
					StatusReason: workflowPermutationReason,
					HumanActions: []string{"approve", "reject"},
				},
				Next: []Transition{{GoTo: ""}},
			},
		},
	}
}

func workflowPermutationComplete(childID string) workflowPermutationEvent {
	return workflowPermutationEvent{
		name: "complete_" + childID,
		apply: func(t *testing.T, scenario *workflowPermutationScenario) {
			t.Helper()
			scenario.completeChild(childID)
		},
	}
}

func workflowPermutationDuplicate(childID string) workflowPermutationEvent {
	return workflowPermutationEvent{
		name: "duplicate_" + childID,
		apply: func(t *testing.T, scenario *workflowPermutationScenario) {
			t.Helper()
			scenario.completeChild(childID)
		},
	}
}

func workflowPermutationRestart() workflowPermutationEvent {
	return workflowPermutationEvent{
		name: "restart",
		apply: func(t *testing.T, scenario *workflowPermutationScenario) {
			t.Helper()
			scenario.restart()
		},
	}
}

func workflowPermutationConcurrentStatusChange(workers int) workflowPermutationEvent {
	return workflowPermutationEvent{
		name: "concurrent_status_change",
		apply: func(t *testing.T, scenario *workflowPermutationScenario) {
			t.Helper()
			scenario.tasks.SetStatus(workflowPermutationTaskID, workflowPermutationStatus)

			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					scenario.engine.HandleStatusChange(workflowPermutationTaskID, workflowPermutationStatus)
				}()
			}
			wg.Wait()
			scenario.assertParallelPending("concurrent status reconcile")
		},
	}
}

func (s *workflowPermutationScenario) completeChild(childID string) {
	s.t.Helper()

	agentID, provider := s.childRoute(childID)
	s.engine.HandleAgentComplete(workflowPermutationTaskID, AgentCompletion{
		AgentID:  agentID,
		Provider: provider,
		Result:   childID + " complete",
		Success:  true,
	})
}

func (s *workflowPermutationScenario) childRoute(childID string) (string, string) {
	s.t.Helper()

	if route, ok := s.childRoutes[childID]; ok {
		return route.AgentID, route.Provider
	}
	ti := s.tasks.mustGetTask(s.t, workflowPermutationTaskID)
	if ti.Workflow == nil {
		s.t.Fatalf("task %s has no workflow", workflowPermutationTaskID)
	}
	rec := ti.Workflow.ParallelInflight[workflowPermutationParentStep]
	if rec == nil {
		s.t.Fatalf("parallel record missing while resolving %s", childID)
	}
	status := rec.Children[childID]
	if status == nil {
		s.t.Fatalf("child %s missing from parallel record", childID)
	}
	if status.AgentID == "" {
		s.t.Fatalf("child %s has no agent route", childID)
	}
	s.childRoutes[childID] = workflowPermutationChildRoute{
		AgentID:  status.AgentID,
		Provider: status.Provider,
	}
	return status.AgentID, status.Provider
}

func (s *workflowPermutationScenario) restart() {
	s.t.Helper()

	current := s.tasks.mustGetTask(s.t, workflowPermutationTaskID)
	freshTasks := newMemTasks()
	freshTasks.Put(cloneWorkflowTaskInfo(current))

	s.tasks = freshTasks
	s.engine = NewEngine(s.store, s.tasks, s.agents, discardLogger())
	s.restoreRoutes()
}

func (s *workflowPermutationScenario) restoreRoutes() {
	s.t.Helper()

	ti := s.tasks.mustGetTask(s.t, workflowPermutationTaskID)
	if ti.Workflow == nil {
		return
	}

	s.engine.mu.Lock()
	defer s.engine.mu.Unlock()
	for _, rec := range ti.Workflow.ParallelInflight {
		if rec == nil {
			continue
		}
		for childID, child := range rec.Children {
			if child == nil || child.AgentID == "" || child.Status != "pending" {
				continue
			}
			s.engine.agentRoutes[child.AgentID] = agentRoute{
				taskID: workflowPermutationTaskID,
				stepID: childID,
			}
		}
	}
}

func (s *workflowPermutationScenario) assertParallelPending(context string) {
	s.t.Helper()

	wf := mustWorkflow(s.t, s.tasks, workflowPermutationTaskID)
	if wf.CurrentStep != workflowPermutationParentStep {
		s.t.Fatalf("%s: current step = %q, want %q", context, wf.CurrentStep, workflowPermutationParentStep)
	}
	if wf.State != ExecWaiting {
		s.t.Fatalf("%s: workflow state = %q, want %q", context, wf.State, ExecWaiting)
	}
	rec := wf.ParallelInflight[workflowPermutationParentStep]
	if rec == nil {
		s.t.Fatalf("%s: parallel record missing", context)
	}
	if rec.AllChildrenDone() {
		s.t.Fatalf("%s: parallel record already terminal", context)
	}
}

func (s *workflowPermutationScenario) result() workflowPermutationResult {
	s.t.Helper()

	ti := s.tasks.mustGetTask(s.t, workflowPermutationTaskID)
	if ti.Workflow == nil {
		s.t.Fatalf("task %s has no workflow", workflowPermutationTaskID)
	}

	return workflowPermutationResult{
		EffectCount: s.agents.CallCount(),
		Effects:     workflowPermutationEffects(s.agents),
		State:       workflowPermutationStateForTask(ti, s.engine),
	}
}

func workflowPermutationEffects(agents *mockAgents) []string {
	agents.mu.Lock()
	defer agents.mu.Unlock()

	seen := make(map[string]struct{}, len(agents.calls))
	for _, call := range agents.calls {
		identity := call.Role + "|" + call.Provider + "|" + call.Mode
		seen[identity] = struct{}{}
	}

	out := make([]string, 0, len(seen))
	for identity := range seen {
		out = append(out, identity)
	}
	slices.Sort(out)
	return out
}

func workflowPermutationStateForTask(ti TaskInfo, engine *Engine) workflowPermutationState {
	wf := ti.Workflow

	state := workflowPermutationState{
		TaskStatus:       ti.Status,
		TaskStatusReason: ti.StatusReason,
		WorkflowID:       wf.WorkflowID,
		CurrentStep:      wf.CurrentStep,
		ExecState:        wf.State,
		StepHistory:      workflowPermutationStepHistory(wf),
		StepCounts:       workflowPermutationStepCounts(wf),
		Variables:        workflowPermutationVars(wf),
		ParallelInflight: workflowPermutationParallelState(wf),
		AgentRoutes:      workflowPermutationAgentRoutes(engine),
	}

	return state
}

func workflowPermutationStepHistory(wf *Execution) []string {
	out := make([]string, 0, len(wf.StepHistory))
	for _, record := range wf.StepHistory {
		entry := record.StepID + "|" + record.Status
		if record.Output != "" {
			entry += "|" + record.Output
		}
		out = append(out, entry)
	}
	return out
}

func workflowPermutationStepCounts(wf *Execution) []string {
	if len(wf.StepCounts) == 0 {
		return nil
	}
	out := make([]string, 0, len(wf.StepCounts))
	for stepID, count := range wf.StepCounts {
		out = append(out, stepID+"="+itoa(count))
	}
	slices.Sort(out)
	return out
}

func workflowPermutationVars(wf *Execution) []string {
	if len(wf.Variables) == 0 {
		return nil
	}
	out := make([]string, 0, len(wf.Variables))
	for key, value := range wf.Variables {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)
	return out
}

func workflowPermutationParallelState(wf *Execution) []string {
	if len(wf.ParallelInflight) == 0 {
		return nil
	}
	out := make([]string, 0, len(wf.ParallelInflight))
	for parentID, rec := range wf.ParallelInflight {
		if rec == nil {
			out = append(out, parentID+"=<nil>")
			continue
		}
		children := make([]string, 0, len(rec.Children))
		for childID, child := range rec.Children {
			if child == nil {
				children = append(children, childID+"=<nil>")
				continue
			}
			children = append(children, childID+"="+child.Status)
		}
		slices.Sort(children)
		out = append(out, parentID+":"+join(children))
	}
	slices.Sort(out)
	return out
}

func workflowPermutationAgentRoutes(engine *Engine) []string {
	engine.mu.Lock()
	defer engine.mu.Unlock()

	if len(engine.agentRoutes) == 0 {
		return nil
	}
	out := make([]string, 0, len(engine.agentRoutes))
	for agentID, route := range engine.agentRoutes {
		out = append(out, agentID+"="+route.taskID+"/"+route.stepID)
	}
	slices.Sort(out)
	return out
}

func assertWorkflowPermutationSettled(t *testing.T, result workflowPermutationResult) {
	t.Helper()

	if result.EffectCount != 2 {
		t.Fatalf("effect count = %d, want 2", result.EffectCount)
	}
	wantEffects := []string{"child_a|claude|headless", "child_b|codex|headless"}
	if !reflect.DeepEqual(result.Effects, wantEffects) {
		t.Fatalf("effects = %#v, want %#v", result.Effects, wantEffects)
	}
	if result.State.TaskStatus != workflowPermutationStatus {
		t.Fatalf("task status = %q, want %q", result.State.TaskStatus, workflowPermutationStatus)
	}
	if result.State.TaskStatusReason != workflowPermutationReason {
		t.Fatalf("task status reason = %q, want %q", result.State.TaskStatusReason, workflowPermutationReason)
	}
	if result.State.CurrentStep != workflowPermutationGateStep {
		t.Fatalf("current step = %q, want %q", result.State.CurrentStep, workflowPermutationGateStep)
	}
	if result.State.ExecState != ExecWaiting {
		t.Fatalf("exec state = %q, want %q", result.State.ExecState, ExecWaiting)
	}
	if result.State.ParallelInflight != nil {
		t.Fatalf("parallel inflight = %#v, want nil", result.State.ParallelInflight)
	}
	if result.State.AgentRoutes != nil {
		t.Fatalf("agent routes = %#v, want nil", result.State.AgentRoutes)
	}
	wantHistory := []string{
		"fan_out|completed|child_a=completed, child_b=completed",
		"set_review|completed",
	}
	if !reflect.DeepEqual(result.State.StepHistory, wantHistory) {
		t.Fatalf("step history = %#v, want %#v", result.State.StepHistory, wantHistory)
	}
}

func cloneWorkflowTaskInfo(ti TaskInfo) TaskInfo {
	cloned := ti
	cloned.Tags = slices.Clone(ti.Tags)
	cloned.Attachments = slices.Clone(ti.Attachments)
	cloned.PlanDrafts = maps.Clone(ti.PlanDrafts)
	cloned.AgentRuns = slices.Clone(ti.AgentRuns)
	if ti.Workflow != nil {
		cloned.Workflow = ti.Workflow.Clone()
	}
	return cloned
}

func join(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for i := 1; i < len(parts); i++ {
		out += "," + parts[i]
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
