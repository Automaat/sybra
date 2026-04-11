package watchdog

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/task"
)

const (
	TickInterval   = 30 * time.Second
	StallLimit     = 3 * time.Minute
	Debounce       = 5 * time.Minute
	InspectTimeout = 2 * time.Minute
)

// sizeBudget returns the maximum total runtime for a headless agent based on
// its task's size tag. Trigger inspection once total runtime exceeds this.
func sizeBudget(tags []string) time.Duration {
	switch {
	case slices.Contains(tags, "large"):
		return 3 * time.Hour
	case slices.Contains(tags, "small"):
		return 10 * time.Minute
	default: // medium or unset
		return 45 * time.Minute
	}
}

type state struct {
	mu             sync.Mutex
	lastInspection map[string]time.Time
}

func newState() *state {
	return &state{lastInspection: make(map[string]time.Time)}
}

func (s *state) shouldInspect(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastInspection[id]
	if ok && now.Sub(last) < Debounce {
		return false
	}
	s.lastInspection[id] = now
	return true
}

// Watchdog monitors headless agents for stalls and budget overruns.
type Watchdog struct {
	agents *agent.Manager
	tasks  *task.Manager
	logger *slog.Logger
	emit   func(string, any)
	wg     *sync.WaitGroup
}

// New creates a Watchdog.
func New(
	agents *agent.Manager,
	tasks *task.Manager,
	logger *slog.Logger,
	emit func(string, any),
	wg *sync.WaitGroup,
) *Watchdog {
	return &Watchdog{
		agents: agents,
		tasks:  tasks,
		logger: logger,
		emit:   emit,
		wg:     wg,
	}
}

// Run blocks until ctx is cancelled, ticking every TickInterval.
func (w *Watchdog) Run(ctx context.Context) {
	s := newState()
	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.tick(ctx, s, now)
		}
	}
}

func (w *Watchdog) tick(ctx context.Context, s *state, now time.Time) {
	for _, ag := range w.agents.ListAgents() {
		if ag.GetState() != agent.StateRunning || ag.Mode != "headless" || ag.External {
			continue
		}
		logPath := ag.GetLogPath()
		if logPath == "" {
			continue
		}

		stall := now.Sub(ag.GetLastEventAt())
		total := now.Sub(ag.StartedAt)

		t, err := w.tasks.Get(ag.TaskID)
		var budget time.Duration
		if err == nil {
			budget = sizeBudget(t.Tags)
		} else {
			budget = sizeBudget(nil)
		}

		trigger := ""
		switch {
		case stall > StallLimit:
			trigger = "stall"
		case total > budget:
			trigger = "budget"
		}
		if trigger == "" {
			continue
		}
		if !s.shouldInspect(ag.ID, now) {
			continue
		}

		w.logger.Info("agent.watchdog.inspect",
			"id", ag.ID, "trigger", trigger,
			"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))

		w.wg.Go(func() { w.inspect(ctx, ag, t, int(stall.Seconds()), int(total.Seconds())) })
	}
}

func (w *Watchdog) inspect(ctx context.Context, ag *agent.Agent, t task.Task, stallSec, totalSec int) {
	ictx, cancel := context.WithTimeout(ctx, InspectTimeout)
	defer cancel()

	verdict, err := agent.Inspect(ictx, agent.InspectInput{
		AgentID:   ag.ID,
		TaskTitle: t.Title,
		LogPath:   ag.GetLogPath(),
		StallSec:  stallSec,
		TotalSec:  totalSec,
	})
	if err != nil {
		w.logger.Warn("agent.watchdog.inspect.failed", "id", ag.ID, "err", err)
		return
	}

	w.logger.Info("agent.watchdog.verdict",
		"id", ag.ID, "stuck", verdict.Stuck,
		"recommendation", verdict.Recommendation, "reason", verdict.Reason)

	w.emit(events.AgentStuck(ag.ID), verdict)

	switch verdict.Recommendation {
	case "stop":
		if err := w.agents.StopAgent(ag.ID); err != nil {
			w.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
		}
		if ag.TaskID != "" {
			if _, err := w.tasks.Update(ag.TaskID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
				w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
	case "escalate":
		if ag.TaskID != "" {
			if _, err := w.tasks.Update(ag.TaskID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
				w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
	case "continue":
		// intentional no-op; debounce suppresses re-check
	}
}
