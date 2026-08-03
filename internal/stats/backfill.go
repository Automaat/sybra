package stats

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/runacct"
	"github.com/Automaat/sybra/internal/skillattr"
)

// Backfill imports historical agent runs from audit logs into the stats
// store. It skips import if the store already has records.
//
// The in-memory Len() check is a fast path only, so backfill is a no-op cost
// after the first successful run instead of scanning the full audit dir on
// every startup. The authoritative guard is the reload+recheck inside the
// lock below: the has-records guard and the append are one critical section,
// flocked across it. Reloading is needed only for this startup backfill path;
// ordinary Record calls append without decoding the history.
func (s *Store) Backfill(auditDir string) error {
	if s.Len() > 0 {
		return nil
	}

	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Now().Add(24 * time.Hour),
		Type:  "agent.",
	})
	if err != nil {
		return err
	}

	unlock, err := fsutil.LockFileWithin(s.path, storeLockTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if len(s.runs) > 0 {
		return nil
	}

	records := backfillRecords(events)
	s.runs = append(s.runs, records...)

	if err := s.appendLocked(records); err != nil {
		return err
	}
	return nil
}

func backfillRecords(events []audit.Event) []RunRecord {
	runs := audit.NormalizeAgentRuns(events)
	records := audit.RunRecords(events)
	out := make([]RunRecord, 0, len(records))
	recordIdx := 0
	for i := range runs {
		run := &runs[i]
		if !run.Terminal || recordIdx >= len(records) {
			continue
		}
		out = append(out, backfillRecord(*run, records[recordIdx]))
		recordIdx++
	}
	return out
}

func backfillRecord(run audit.RunLifecycle, rec runacct.Record) RunRecord {
	r := RunRecord{
		ID:                 rec.ID,
		TaskID:             rec.TaskID,
		Mode:               rec.Mode,
		Role:               rec.Role,
		Model:              rec.Model,
		Provider:           rec.Provider,
		ReasoningEffort:    rec.ReasoningEffort,
		ExperimentID:       rec.ExperimentID,
		VariantID:          rec.VariantID,
		CostUSD:            rec.CostUSD,
		DurationS:          rec.DurationS,
		PremiumRequests:    rec.PremiumRequests,
		TurnCount:          rec.TurnCount,
		ToolCalls:          rec.ToolCalls,
		Outcome:            rec.Outcome,
		SkillExecutionMode: skillattr.ExecutionModeUnknown,
		SkillConformance:   skillattr.ConformanceUnknown,
		Timestamp:          rec.Timestamp,
	}
	if v, ok := run.TerminalEvent.Data["reasoning_tokens"].(float64); ok {
		r.ReasoningTokens = int(v)
	}
	if v, ok := run.TerminalEvent.Data["input_tokens"].(float64); ok {
		r.InputTokens = int(v)
	}
	if v, ok := run.TerminalEvent.Data["output_tokens"].(float64); ok {
		r.OutputTokens = int(v)
	}
	if v, ok := run.TerminalEvent.Data["cache_creation_input_tokens"].(float64); ok {
		r.CacheCreationInputTokens = int(v)
	}
	if v, ok := run.TerminalEvent.Data["cache_read_input_tokens"].(float64); ok {
		r.CacheReadInputTokens = int(v)
	}
	if v, ok := run.TerminalEvent.Data["premium_requests"].(float64); ok {
		r.PremiumRequests = v
	}
	if v, ok := run.TerminalEvent.Data["turn_count"].(float64); ok {
		r.TurnCount = int(v)
	}
	if v, ok := run.TerminalEvent.Data["tool_calls"].(float64); ok {
		r.ToolCalls = int(v)
	}
	if v, ok := run.TerminalEvent.Data["requested_skill"].(string); ok {
		r.RequestedSkill = v
	}
	if v, ok := run.TerminalEvent.Data["skill_execution_mode"].(string); ok && v != "" {
		r.SkillExecutionMode = v
	}
	if v, ok := run.TerminalEvent.Data["resolved_skill_source_hash"].(string); ok {
		r.ResolvedSkillSourceHash = v
	}
	if v, ok := run.TerminalEvent.Data["skill_conformance"].(string); ok && v != "" {
		r.SkillConformance = v
	}
	return r
}
