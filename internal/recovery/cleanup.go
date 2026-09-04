package recovery

import (
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

// cleanStaleRuns marks agent_runs still showing "running" as "stopped" if
// no matching in-memory agent exists. Fixes leftover state from
// crashes/restarts.
func (r *Recovery) cleanStaleRuns() {
	tasks, err := r.Tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		for j := range tasks[i].AgentRuns {
			run := &tasks[i].AgentRuns[j]
			if run.State != string(agent.StateRunning) {
				continue
			}
			if r.Agents.HasRunningAgentForTask(tasks[i].ID) {
				continue
			}
			r.Logger.Info("stale-run.cleanup", "task_id", tasks[i].ID, "agent_id", run.AgentID)
			_ = r.Tasks.UpdateRunBy(tasks[i].ID, "recovery.cleanup.stale_run", run.AgentID, task.RunPatch{
				State:  task.Ptr(string(agent.StateStopped)),
				Result: task.Ptr("stale: marked stopped on startup"),
			})
		}
	}
}
