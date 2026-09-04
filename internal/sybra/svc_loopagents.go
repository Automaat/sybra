package sybra

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/audit"

	"github.com/Automaat/sybra/internal/loopagent"
)

// LoopAgentService exposes loop-agent CRUD + run history as Wails-bound
// methods. The scheduler owns the actual ticking goroutines; this service
// is the persistence + GUI surface.
type LoopAgentService struct {
	ctx    context.Context //nolint:containedctx // bound methods take no context; see wireLoopAgentService
	store  loopagent.Repository
	sched  *loopagent.Scheduler
	audit  audit.Store
	logger *slog.Logger
}

// LoopAgentRun is a single fire of a loop agent, materialized from the
// audit log on demand. We don't keep a separate run history store —
// existing audit + per-agent ndjson logging is the source of truth.
type LoopAgentRun struct {
	AgentID    string    `json:"agentId"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	CostUSD    float64   `json:"costUsd"`
	State      string    `json:"state"`
	DurationS  float64   `json:"durationS"`
}

// context returns the app context, falling back to Background for the
// pre-startup service struct Wails binds before dependencies are wired.
func (s *LoopAgentService) context() context.Context {
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// ListLoopAgents returns every persisted loop agent, sorted by name.
func (s *LoopAgentService) ListLoopAgents() ([]loopagent.LoopAgent, error) {
	return s.store.List(s.context())
}

// GetLoopAgent returns one loop agent by ID.
func (s *LoopAgentService) GetLoopAgent(id string) (loopagent.LoopAgent, error) {
	return s.store.Get(s.context(), id)
}

// CreateLoopAgent persists a new loop agent and reconciles the scheduler.
func (s *LoopAgentService) CreateLoopAgent(la loopagent.LoopAgent) (loopagent.LoopAgent, error) {
	created, err := s.store.Create(s.context(), la)
	if err != nil {
		return loopagent.LoopAgent{}, err
	}
	s.logger.Info("loopagent.create", "id", created.ID, "name", created.Name, "interval_s", created.IntervalSec)
	s.sched.Sync()
	return created, nil
}

// UpdateLoopAgent persists changes and reconciles the scheduler. Interval
// changes take effect on the next Sync.
func (s *LoopAgentService) UpdateLoopAgent(la loopagent.LoopAgent) (loopagent.LoopAgent, error) {
	updated, err := s.store.Update(s.context(), la)
	if err != nil {
		return loopagent.LoopAgent{}, err
	}
	s.logger.Info("loopagent.update", "id", updated.ID, "enabled", updated.Enabled)
	s.sched.Sync()
	return updated, nil
}

// DeleteLoopAgent removes the record and stops the running fetcher (if any).
func (s *LoopAgentService) DeleteLoopAgent(id string) error {
	if err := s.store.Delete(s.context(), id); err != nil {
		return err
	}
	s.logger.Info("loopagent.delete", "id", id)
	s.sched.Sync()
	return nil
}

// RunLoopAgentNow fires a loop agent once immediately, regardless of its
// configured schedule. The next scheduled tick still happens at its natural
// time. Returns the spawned agent ID for the GUI to follow.
func (s *LoopAgentService) RunLoopAgentNow(id string) (string, error) {
	agentID, err := s.sched.RunNow(id)
	if err != nil {
		return "", err
	}
	s.logger.Info("loopagent.run-now", "id", id, "agent_id", agentID)
	return agentID, nil
}

// ListLoopAgentRuns returns recent runs of a loop agent, derived from
// audit log entries with name prefix "loop:<name>". Sorted newest-first;
// capped at limit (defaults to 50 when limit <= 0).
func (s *LoopAgentService) ListLoopAgentRuns(id string, limit int) ([]LoopAgentRun, error) {
	if limit <= 0 {
		limit = 50
	}
	la, err := s.store.Get(s.context(), id)
	if err != nil {
		return nil, err
	}
	agentName := la.AgentName()

	// Through the store, not the day-files: under a database backend those
	// stop growing, and a run list built from them would show nothing after
	// the flip while the schedule kept running.
	if s.audit == nil {
		return []LoopAgentRun{}, nil
	}
	until := time.Now().UTC().Add(time.Minute)
	events, err := s.audit.Read(audit.Query{
		Since: until.AddDate(0, 0, -loopAgentRunLookbackDays),
		Until: until,
		Type:  "agent.completed",
	})
	if err != nil {
		return nil, err
	}
	runs := runsFromAuditEvents(events, agentName)

	slices.SortFunc(runs, func(a, b LoopAgentRun) int { return b.FinishedAt.Compare(a.FinishedAt) })
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// loopAgentRunLookbackDays bounds the window a run list is built from. The
// file scan walked every day-file it had; a bounded query is what lets the
// database read only the rows it needs.
const loopAgentRunLookbackDays = 30

// runsFromAuditEvents extracts this agent's completed runs from a trail slice.
func runsFromAuditEvents(events []audit.Event, agentName string) []LoopAgentRun {
	out := make([]LoopAgentRun, 0, len(events))
	for i := range events {
		ev := events[i]
		if ev.Type != "agent.completed" {
			continue
		}
		if name, _ := ev.Data["name"].(string); name != agentName {
			continue
		}
		costFloat, _ := ev.Data["cost_usd"].(float64)
		duration, _ := ev.Data["duration_s"].(float64)
		state, _ := ev.Data["state"].(string)
		out = append(out, LoopAgentRun{
			AgentID:    ev.AgentID,
			StartedAt:  ev.Timestamp.Add(-time.Duration(duration * float64(time.Second))),
			FinishedAt: ev.Timestamp,
			CostUSD:    costFloat,
			State:      state,
			DurationS:  duration,
		})
	}
	return out
}

// auditFilesNewestFirst returns the audit log files (one per day) sorted
// from newest to oldest by filename, which encodes the date.
func auditFilesNewestFirst(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ndjson") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	slices.SortFunc(paths, func(a, b string) int { return cmp.Compare(b, a) })
	return paths, nil
}

// scanAuditFileForRuns extracts agent.completed events whose `name` field
// equals the loop's AgentName ("loop:<name>"). The audit format is one
// JSON object per line. The `name` field is written by the global
// onAgentComplete hook in app.go alongside cost/duration/role.
func scanAuditFileForRuns(path, agentName string) ([]LoopAgentRun, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []LoopAgentRun
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			TS      time.Time      `json:"ts"`
			Type    string         `json:"type"`
			AgentID string         `json:"agent_id"`
			Data    map[string]any `json:"data"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Type != "agent.completed" {
			continue
		}
		name, _ := ev.Data["name"].(string)
		if name != agentName {
			continue
		}
		costFloat, _ := ev.Data["cost_usd"].(float64)
		duration, _ := ev.Data["duration_s"].(float64)
		state, _ := ev.Data["state"].(string)
		started := ev.TS.Add(-time.Duration(duration * float64(time.Second)))
		out = append(out, LoopAgentRun{
			AgentID:    ev.AgentID,
			StartedAt:  started,
			FinishedAt: ev.TS,
			CostUSD:    costFloat,
			State:      state,
			DurationS:  duration,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return out, nil
}
