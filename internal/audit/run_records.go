package audit

import (
	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/runoutcome"
)

func RunRecords(events []Event) []runacct.Record {
	runs := NormalizeAgentRuns(events)
	out := make([]runacct.Record, 0, len(runs))
	for i := range runs {
		run := runs[i]
		if !run.Terminal {
			continue
		}
		rec := runacct.Record{
			ID:        run.AgentID,
			TaskID:    run.TaskID,
			Outcome:   runoutcome.Normalize(run.Outcome),
			Timestamp: run.TerminalAt,
		}
		if v, ok := run.TerminalEvent.Data["role"].(string); ok {
			rec.Role = v
		}
		if v, ok := run.TerminalEvent.Data["mode"].(string); ok {
			rec.Mode = v
		}
		if v, ok := run.TerminalEvent.Data["provider"].(string); ok {
			rec.Provider = v
		}
		if v, ok := run.TerminalEvent.Data["model"].(string); ok {
			rec.Model = v
		}
		if v, ok := run.TerminalEvent.Data["reasoning_effort"].(string); ok {
			rec.ReasoningEffort = v
		}
		if v, ok := run.TerminalEvent.Data["experiment_id"].(string); ok {
			rec.ExperimentID = v
		}
		if v, ok := run.TerminalEvent.Data["variant_id"].(string); ok {
			rec.VariantID = v
		}
		if v, ok := run.TerminalEvent.Data["skill_execution_mode"].(string); ok {
			rec.SkillExecutionMode = v
		}
		if v, ok := run.TerminalEvent.Data["skill_conformance"].(string); ok {
			rec.SkillConformance = v
		}
		if v, ok := run.TerminalEvent.Data["cost_usd"].(float64); ok {
			rec.CostUSD = v
		}
		if v, ok := run.TerminalEvent.Data["duration_s"].(float64); ok {
			rec.DurationS = v
		}
		if v, ok := run.TerminalEvent.Data["premium_requests"].(float64); ok {
			rec.PremiumRequests = v
		}
		if v, ok := run.TerminalEvent.Data["turn_count"].(float64); ok {
			rec.TurnCount = int(v)
		}
		if v, ok := run.TerminalEvent.Data["tool_calls"].(float64); ok {
			rec.ToolCalls = int(v)
		}
		out = append(out, rec)
	}
	return out
}
