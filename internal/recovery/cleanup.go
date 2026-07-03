package recovery

import (
	"context"

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
		if tasks[i].TaskType == task.TaskTypeChat {
			continue
		}
		for j := range tasks[i].AgentRuns {
			run := &tasks[i].AgentRuns[j]
			if run.State != string(agent.StateRunning) {
				continue
			}
			if r.Agents.HasRunningAgentForTask(tasks[i].ID) {
				continue
			}
			r.Logger.Info("stale-run.cleanup", "task_id", tasks[i].ID, "agent_id", run.AgentID)
			_ = r.Tasks.UpdateRun(tasks[i].ID, run.AgentID, task.RunPatch{
				State:  task.Ptr(string(agent.StateStopped)),
				Result: task.Ptr("stale: marked stopped on startup"),
			})
		}
	}
}

// gcOrphanChats deletes any chat-task that no longer has a running agent.
// Chats are ephemeral by design; a stale chat-task is always noise left
// over from a crash or kill. Runs before worktree orphan cleanup so the
// task file is gone by the time the worktree sweeper looks.
func (r *Recovery) gcOrphanChats(ctx context.Context) {
	tasks, err := r.Tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.TaskType != task.TaskTypeChat {
			continue
		}
		if r.Agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		r.Logger.Info("chat.gc.orphan", "task_id", t.ID, "title", t.Title)
		r.Worktrees.Remove(ctx, t.ID)
		if err := r.Tasks.Delete(t.ID); err != nil {
			r.Logger.Error("chat.gc.delete", "task_id", t.ID, "err", err)
		}
	}
}
