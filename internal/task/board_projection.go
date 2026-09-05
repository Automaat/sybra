package task

import "github.com/Automaat/sybra/internal/workflow"

// BoardProjection returns the task fields needed to render and search board
// cards without the historical execution corpus used only by task detail.
// GetTask remains the authoritative full record.
func BoardProjection(t Task) Task {
	t.AgentRuns = boardAgentRuns(t.AgentRuns)
	t.Attachments = nil
	t.EffectLog = nil
	if t.Workflow != nil {
		t.Workflow = &workflow.Execution{
			WorkflowID:  t.Workflow.WorkflowID,
			CurrentStep: t.Workflow.CurrentStep,
			State:       t.Workflow.State,
		}
	}
	return t
}

func boardAgentRuns(runs []AgentRun) []AgentRun {
	if len(runs) == 0 {
		return nil
	}
	out := make([]AgentRun, len(runs))
	for i := range runs {
		run := &runs[i]
		out[i] = AgentRun{
			AgentID:   run.AgentID,
			Role:      run.Role,
			State:     run.State,
			StartedAt: run.StartedAt,
			CostUSD:   run.CostUSD,
		}
	}
	return out
}
