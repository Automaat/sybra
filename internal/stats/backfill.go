package stats

import (
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/fsutil"
)

// Backfill imports historical agent runs from audit logs into the stats
// store. It skips import if the store already has records.
//
// The has-records guard and the append are one read-modify-write critical
// section, flocked across it: without the reload immediately inside the
// lock, a concurrent Record from another process (or another instance of
// Backfill) between this process's stale in-memory check and its flush would
// be silently discarded by the overwrite.
func (s *Store) Backfill(auditDir string) error {
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

	for _, ev := range events {
		if ev.Type != audit.EventAgentCompleted && ev.Type != audit.EventAgentFailed {
			continue
		}

		r := RunRecord{
			ID:        ev.AgentID,
			TaskID:    ev.TaskID,
			Timestamp: ev.Timestamp,
		}

		if v, ok := ev.Data["mode"].(string); ok {
			r.Mode = v
		}
		if v, ok := ev.Data["cost_usd"].(float64); ok {
			r.CostUSD = v
		}
		if v, ok := ev.Data["duration_s"].(float64); ok {
			r.DurationS = v
		}
		if v, ok := ev.Data["state"].(string); ok && v == "stopped" {
			r.Outcome = "completed"
		} else {
			r.Outcome = "failed"
		}
		if v, ok := ev.Data["provider"].(string); ok {
			r.Provider = v
		}
		if v, ok := ev.Data["reasoning_tokens"].(float64); ok {
			r.ReasoningTokens = int(v)
		}

		s.runs = append(s.runs, r)
	}

	if len(s.runs) > 0 {
		return s.flush()
	}
	return nil
}
