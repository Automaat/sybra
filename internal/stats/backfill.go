package stats

import (
	"time"

	"github.com/Automaat/synapse/internal/audit"
)

// Backfill imports historical agent runs from audit logs into the stats store.
// It skips import if the store already has records.
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

		s.runs = append(s.runs, r)
	}

	if len(s.runs) > 0 {
		return s.flush()
	}
	return nil
}
