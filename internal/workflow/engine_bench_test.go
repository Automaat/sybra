package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// newBenchStore creates a Store backed by a temp dir, copying test-simple.yaml.
func newBenchStore(b *testing.B) *Store {
	b.Helper()
	dir := b.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		b.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "test-simple.yaml"))
	if err != nil {
		b.Fatalf("read test workflow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test-simple.yaml"), src, 0o644); err != nil {
		b.Fatal(err)
	}
	return store
}

// BenchmarkStartWorkflow measures workflow initialization: store lookup,
// trigger condition evaluation, and spawning the first agent.
func BenchmarkStartWorkflow(b *testing.B) {
	store := newBenchStore(b)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	b.ResetTimer()
	for i := range b.N {
		id := fmt.Sprintf("t%d", i)
		tasks.Put(TaskInfo{ID: id, Status: "todo", AgentMode: "headless"})
		if err := engine.StartWorkflow(id, "test-simple"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAdvanceStep_ToImplement measures AdvanceStep driving
// triage → set_in_progress (sync) → implement (async agent spawn).
// One synchronous set_status step executes inline per iteration.
func BenchmarkAdvanceStep_ToImplement(b *testing.B) {
	store := newBenchStore(b)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	b.ResetTimer()
	for i := range b.N {
		id := fmt.Sprintf("t%d", i)
		tasks.Put(TaskInfo{ID: id, Status: "todo", AgentMode: "headless"})
		if err := engine.StartWorkflow(id, "test-simple"); err != nil {
			b.Fatal(err)
		}
		agentID := agents.LastID()
		agents.SimulateComplete(id)
		if err := engine.AdvanceStep(id, StepOutput{
			StepID: "triage", Status: "completed", AgentID: agentID,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAdvanceStep_Retry measures the retry path: triage fails and the
// engine re-executes the same step (max_retries=3 in test-simple.yaml).
func BenchmarkAdvanceStep_Retry(b *testing.B) {
	store := newBenchStore(b)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	b.ResetTimer()
	for i := range b.N {
		id := fmt.Sprintf("t%d", i)
		tasks.Put(TaskInfo{ID: id, Status: "todo", AgentMode: "headless"})
		if err := engine.StartWorkflow(id, "test-simple"); err != nil {
			b.Fatal(err)
		}
		agentID := agents.LastID()
		agents.SimulateComplete(id)
		if err := engine.AdvanceStep(id, StepOutput{
			StepID: "triage", Status: "failed", AgentID: agentID,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkResumeStalled measures scanning a pool of stalled tasks and
// re-dispatching them. Exercises ListTasks, per-task state checks, and
// the inflightMutex map under realistic task counts.
func BenchmarkResumeStalled(b *testing.B) {
	const taskCount = 20
	store := newBenchStore(b)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	for i := range taskCount {
		id := fmt.Sprintf("stalled-%d", i)
		tasks.Put(TaskInfo{ID: id, Status: "todo", AgentMode: "headless"})
		if err := engine.StartWorkflow(id, "test-simple"); err != nil {
			b.Fatal(err)
		}
		agents.SimulateComplete(id)
	}

	b.ResetTimer()
	for range b.N {
		engine.ResumeStalled()
	}
}

// BenchmarkConcurrentAdvance_DistinctTasks measures concurrent AdvanceStep
// calls across independent tasks, exercising the per-task inflightMutex map
// under goroutine contention without same-ID collisions.
func BenchmarkConcurrentAdvance_DistinctTasks(b *testing.B) {
	store := newBenchStore(b)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())

	var counter atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := fmt.Sprintf("par-t%d", counter.Add(1))
			tasks.Put(TaskInfo{ID: id, Status: "todo", AgentMode: "headless"})
			if err := engine.StartWorkflow(id, "test-simple"); err != nil {
				b.Error(err)
				continue
			}
			agentID := agents.RunningAgentID(id)
			agents.SimulateComplete(id)
			if err := engine.AdvanceStep(id, StepOutput{
				StepID: "triage", Status: "completed", AgentID: agentID,
			}); err != nil {
				b.Error(err)
			}
		}
	})
}
