package stats

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/fsutil"
	"github.com/Automaat/sybra/internal/skillattr"
)

// Backfill imports historical agent runs from audit logs into the stats
// store. It skips import if the store already has records.
//
// The in-memory Len() check is a fast path only, so backfill is a no-op cost
// after the first successful run instead of scanning the full audit dir on
// every startup. The authoritative guard is the reload+recheck inside the
// lock below: the has-records guard and the append are one read-modify-write
// critical section, flocked across it. Without the reload immediately inside
// the lock, a concurrent Record from another process (or another instance of
// Backfill) between this process's stale in-memory check and its flush would
// be silently discarded by the overwrite.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	unlock, err := fsutil.LockFile(s.path)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()

	if err := s.reloadLocked(); err != nil {
		return err
	}
	if len(s.runs) > 0 {
		return nil
	}

	runs := audit.NormalizeAgentRuns(events)
	for i := range runs {
		run := &runs[i]
		if !run.Terminal {
			continue
		}

		r := RunRecord{
			ID:                 run.AgentID,
			TaskID:             run.TaskID,
			SkillExecutionMode: skillattr.ExecutionModeUnknown,
			SkillConformance:   skillattr.ConformanceUnknown,
			Timestamp:          run.TerminalAt,
		}

		if v, ok := run.TerminalEvent.Data["mode"].(string); ok {
			r.Mode = v
		}
		if v, ok := run.TerminalEvent.Data["cost_usd"].(float64); ok {
			r.CostUSD = v
		}
		if v, ok := run.TerminalEvent.Data["duration_s"].(float64); ok {
			r.DurationS = v
		}
		if run.Failed {
			r.Outcome = "failed"
		} else {
			r.Outcome = "completed"
		}
		if v, ok := run.TerminalEvent.Data["provider"].(string); ok {
			r.Provider = v
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

		s.runs = append(s.runs, r)
	}

	if len(s.runs) > 0 {
		return s.flush()
	}
	return nil
}
