package workflow

import (
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/taskstatus"
)

const workflowPermutationContractYAML = `id: permutation-contract
name: Permutation Contract
trigger:
  on: manual
steps:
  - id: plan
    type: parallel
    parallel:
      - id: plan_a
        type: run_agent
        config:
          role: plan
          provider: claude
          mode: headless
          prompt: "Plan A {{.Task.ID}}"
      - id: plan_b
        type: run_agent
        config:
          role: plan
          provider: codex
          mode: headless
          prompt: "Plan B {{.Task.ID}}"
    next:
      - goto: set_review
  - id: set_review
    type: set_status
    config:
      status: plan-review
      status_reason: ready for review
    next:
      - goto: review_plan
  - id: review_plan
    type: wait_human
    config:
      status: plan-review
      human_actions: [approve, reject]
    next:
      - goto: ""
`

type workflowPermutationScenario struct {
	t      *testing.T
	store  *Store
	tasks  *memTasks
	agents *mockAgents
	engine *Engine
}

type workflowPermutationEvent struct {
	Name string
	Fire func(*workflowPermutationScenario)
}

type workflowPermutationResult struct {
	TaskStatus       string
	TaskReason       string
	WorkflowID       string
	CurrentStep      string
	ExecState        ExecState
	StepCounts       []string
	Variables        []string
	ParallelChildren []string
	EffectSet        []string
	EffectCounts     []string
}

func TestWorkflowPermutationContract(t *testing.T) {
	prev := providerAvailable
	providerAvailable = func(string) bool { return true }
	defer func() { providerAvailable = prev }()

	baseline := runWorkflowPermutationContract(t, workflowPermutationBaselineEvents())
	withDuplicates := runWorkflowPermutationContract(t, workflowPermutationEvents())
	assertWorkflowPermutationEqual(t, "duplicate-control", baseline, withDuplicates)

	perms := sampleWorkflowPermutationOrders(workflowPermutationEvents(), 32, 42)
	for i, perm := range perms {
		names := workflowPermutationEventNames(perm)
		t.Run(fmt.Sprintf("perm-%02d/%s", i, strings.Join(names, ",")), func(t *testing.T) {
			got := runWorkflowPermutationContract(t, perm)
			assertWorkflowPermutationEqual(t, strings.Join(names, ","), baseline, got)
		})
	}

	t.Run("concurrent-reconciler", func(t *testing.T) {
		for i := range 100 {
			t.Run(fmt.Sprintf("iter-%03d", i), func(t *testing.T) {
				s := newWorkflowPermutationScenario(t)
				s.complete("agent-1", "claude", "plan a done")
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					s.statusChange("plan-review")
				}()
				go func() {
					defer wg.Done()
					s.complete("agent-2", "codex", "plan b done")
				}()
				wg.Wait()
				assertWorkflowPermutationEqual(t, "concurrent", baseline, s.result())
			})
		}
	})
}

func newWorkflowPermutationScenario(t *testing.T) *workflowPermutationScenario {
	t.Helper()
	store := newInlineTestStore(t, "permutation-contract", workflowPermutationContractYAML)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "todo", AgentMode: "headless"})
	if err := engine.StartWorkflow("t1", "permutation-contract"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
		panic("unreachable")
	}
	return &workflowPermutationScenario{t: t, store: store, tasks: tasks, agents: agents, engine: engine}
}

func (s *workflowPermutationScenario) complete(agentID, provider, result string) {
	s.engine.HandleAgentComplete("t1", AgentCompletion{
		AgentID:  agentID,
		Provider: provider,
		Result:   result,
		Success:  true,
	})
}

func (s *workflowPermutationScenario) statusChange(status taskstatus.Status) {
	ti := mustTaskInfo(s.t, s.tasks, "t1")
	if err := s.tasks.UpdateTaskStatus("t1", status, ti.StatusReason); err != nil {
		s.t.Fatalf("UpdateTaskStatus: %v", err)
	}
	s.engine.HandleStatusChange("t1", string(status))
}

func (s *workflowPermutationScenario) restart() {
	s.t.Helper()
	clonedTasks := newMemTasks()
	tasks := mustListTasks(s.t, s.tasks)
	for i := range tasks {
		clonedTasks.Put(cloneWorkflowPermutationTask(tasks[i]))
	}
	clonedAgents := cloneWorkflowPermutationAgents(s.agents)
	clonedEngine := NewTestEngine(s.store, clonedTasks, clonedAgents, discardLogger())
	rehydrateWorkflowPermutationRoutes(clonedEngine, clonedTasks)
	s.tasks = clonedTasks
	s.agents = clonedAgents
	s.engine = clonedEngine
}

func (s *workflowPermutationScenario) result() workflowPermutationResult {
	ti := mustTaskInfo(s.t, s.tasks, "t1")
	result := workflowPermutationResult{TaskStatus: string(ti.Status), TaskReason: ti.StatusReason}
	if ti.Workflow == nil {
		return result
	}
	result.WorkflowID = ti.Workflow.WorkflowID
	result.CurrentStep = ti.Workflow.CurrentStep
	result.ExecState = ti.Workflow.State
	result.StepCounts = workflowPermutationStepCounts(ti.Workflow)
	result.Variables = workflowPermutationVars(ti.Workflow.Variables)
	result.ParallelChildren = workflowPermutationParallelChildren(ti.Workflow)
	result.EffectSet, result.EffectCounts = workflowPermutationEffects(s.agents.calls)
	return result
}

func runWorkflowPermutationContract(t *testing.T, events []workflowPermutationEvent) workflowPermutationResult {
	t.Helper()
	s := newWorkflowPermutationScenario(t)
	for _, event := range events {
		event.Fire(s)
	}
	return s.result()
}

func workflowPermutationBaselineEvents() []workflowPermutationEvent {
	return []workflowPermutationEvent{
		{Name: "status-change", Fire: func(s *workflowPermutationScenario) { s.statusChange("plan-review") }},
		{Name: "child-a-complete", Fire: func(s *workflowPermutationScenario) { s.complete("agent-1", "claude", "plan a done") }},
		{Name: "restart", Fire: func(s *workflowPermutationScenario) { s.restart() }},
		{Name: "concurrent-status-child-b", Fire: func(s *workflowPermutationScenario) {
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				s.statusChange("plan-review")
			}()
			go func() {
				defer wg.Done()
				s.complete("agent-2", "codex", "plan b done")
			}()
			wg.Wait()
		}},
	}
}

func workflowPermutationEvents() []workflowPermutationEvent {
	events := workflowPermutationBaselineEvents()
	return []workflowPermutationEvent{
		events[0],
		{Name: "status-change-dup", Fire: events[0].Fire},
		events[1],
		{Name: "child-a-complete-dup", Fire: events[1].Fire},
		events[2],
		events[3],
	}
}

func sampleWorkflowPermutationOrders(events []workflowPermutationEvent, limit int, seed int64) [][]workflowPermutationEvent {
	if permutationCountAtMost(len(events), limit) {
		return permuteWorkflowPermutationEvents(events)
	}
	rng := rand.New(rand.NewSource(seed))
	seen := make(map[string]bool, limit)
	out := make([][]workflowPermutationEvent, 0, limit)
	for len(out) < limit {
		perm := slices.Clone(events)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		key := strings.Join(workflowPermutationEventNames(perm), ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, perm)
	}
	return out
}

// permutationCountAtMost reports whether len(events)! <= limit without
// materializing every permutation just to count them — the factorial grows
// fast enough that doing so would defeat the point of sampling.
func permutationCountAtMost(n, limit int) bool {
	count := 1
	for i := 2; i <= n; i++ {
		count *= i
		if count > limit {
			return false
		}
	}
	return count <= limit
}

func permuteWorkflowPermutationEvents(events []workflowPermutationEvent) [][]workflowPermutationEvent {
	out := make([][]workflowPermutationEvent, 0)
	cur := slices.Clone(events)
	var walk func(int)
	walk = func(i int) {
		if i == len(cur) {
			out = append(out, slices.Clone(cur))
			return
		}
		for j := i; j < len(cur); j++ {
			cur[i], cur[j] = cur[j], cur[i]
			walk(i + 1)
			cur[i], cur[j] = cur[j], cur[i]
		}
	}
	walk(0)
	return out
}

func workflowPermutationEventNames(events []workflowPermutationEvent) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}

func assertWorkflowPermutationEqual(t *testing.T, name string, want, got workflowPermutationResult) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s result mismatch\nwant: %+v\n got: %+v", name, want, got)
	}
}

func workflowPermutationStepCounts(wf *Execution) []string {
	if wf == nil || len(wf.StepCounts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(wf.StepCounts))
	for key := range wf.StepCounts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%s=%d", key, wf.StepCounts[key]))
	}
	return out
}

func workflowPermutationVars(vars map[string]string) []string {
	if len(vars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vars))
	for key := range vars {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+vars[key])
	}
	return out
}

func workflowPermutationParallelChildren(wf *Execution) []string {
	if wf == nil || len(wf.ParallelInflight) == 0 {
		return nil
	}
	var out []string
	parents := make([]string, 0, len(wf.ParallelInflight))
	for parentID := range wf.ParallelInflight {
		parents = append(parents, parentID)
	}
	slices.Sort(parents)
	for _, parentID := range parents {
		rec := wf.ParallelInflight[parentID]
		if rec == nil || len(rec.Children) == 0 {
			continue
		}
		childIDs := make([]string, 0, len(rec.Children))
		for childID := range rec.Children {
			childIDs = append(childIDs, childID)
		}
		slices.Sort(childIDs)
		for _, childID := range childIDs {
			child := rec.Children[childID]
			if child == nil {
				out = append(out, parentID+"/"+childID+"=<nil>")
				continue
			}
			out = append(out, fmt.Sprintf("%s/%s=%s|%s|%s|%d", parentID, childID, child.Status, child.Provider, child.AgentID, child.Retries))
		}
	}
	return out
}

func workflowPermutationEffects(calls []startCall) (effectSet, effectCounts []string) {
	if len(calls) == 0 {
		return nil, nil
	}
	counts := make(map[string]int)
	for i := range calls {
		call := &calls[i]
		key := strings.Join([]string{workflowPermutationStepID(call.Prompt), call.Provider, call.Role}, "|")
		counts[key]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	countStrings := make([]string, 0, len(keys))
	for _, key := range keys {
		countStrings = append(countStrings, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	effectSet = keys
	effectCounts = countStrings
	return effectSet, effectCounts
}

func workflowPermutationStepID(prompt string) string {
	switch {
	case strings.Contains(prompt, "Plan A"):
		return "plan_a"
	case strings.Contains(prompt, "Plan B"):
		return "plan_b"
	default:
		return "unknown"
	}
}

func cloneWorkflowPermutationTask(task TaskInfo) TaskInfo {
	cloned := task
	cloned.Tags = slices.Clone(task.Tags)
	cloned.Attachments = slices.Clone(task.Attachments)
	cloned.AgentRuns = slices.Clone(task.AgentRuns)
	cloned.PlanDrafts = maps.Clone(task.PlanDrafts)
	cloned.Workflow = task.Workflow.Clone()
	return cloned
}

func cloneWorkflowPermutationAgents(src *mockAgents) *mockAgents {
	src.mu.Lock()
	defer src.mu.Unlock()
	return &mockAgents{
		calls:             slices.Clone(src.calls),
		prompts:           slices.Clone(src.prompts),
		running:           maps.Clone(src.running),
		roles:             maps.Clone(src.roles),
		counter:           src.counter,
		failSpawn:         src.failSpawn,
		providerRateLimit: src.providerRateLimit,
		providerFailover:  src.providerFailover,
		rateLimited:       maps.Clone(src.rateLimited),
		unhealthy:         maps.Clone(src.unhealthy),
		dispatchClaimed:   maps.Clone(src.dispatchClaimed),
		claimInsideStart:  src.claimInsideStart,
		failSpawnOnce:     src.failSpawnOnce,
		admitDenyReason:   src.admitDenyReason,
	}
}

func rehydrateWorkflowPermutationRoutes(engine *Engine, tasks *memTasks) {
	_ = engine
	_ = tasks
}

func mustListTasks(t *testing.T, tasks *memTasks) []TaskInfo {
	t.Helper()
	list, err := tasks.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
		panic("unreachable")
	}
	return list
}
