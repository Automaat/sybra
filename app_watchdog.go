package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/task"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	watchdogTickInterval = 30 * time.Second
	watchdogStallLimit   = 3 * time.Minute
	watchdogDebounce     = 5 * time.Minute
	inspectorTimeout     = 2 * time.Minute
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

type watchdogState struct {
	mu             sync.Mutex
	lastInspection map[string]time.Time
}

func newWatchdogState() *watchdogState {
	return &watchdogState{lastInspection: make(map[string]time.Time)}
}

func (s *watchdogState) shouldInspect(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.lastInspection[id]
	if ok && now.Sub(last) < watchdogDebounce {
		return false
	}
	s.lastInspection[id] = now
	return true
}

func (a *App) agentWatchdogLoop(ctx context.Context) {
	state := newWatchdogState()
	ticker := time.NewTicker(watchdogTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.watchdogTick(ctx, state, now)
		}
	}
}

func (a *App) watchdogTick(ctx context.Context, state *watchdogState, now time.Time) {
	for _, ag := range a.agents.ListAgents() {
		if ag.GetState() != agent.StateRunning || ag.Mode != "headless" || ag.External {
			continue
		}
		logPath := ag.GetLogPath()
		if logPath == "" {
			continue
		}

		stall := now.Sub(ag.GetLastEventAt())
		total := now.Sub(ag.StartedAt)

		t, err := a.tasks.Get(ag.TaskID)
		var budget time.Duration
		if err == nil {
			budget = sizeBudget(t.Tags)
		} else {
			budget = sizeBudget(nil)
		}

		trigger := ""
		switch {
		case stall > watchdogStallLimit:
			trigger = "stall"
		case total > budget:
			trigger = "budget"
		}
		if trigger == "" {
			continue
		}
		if !state.shouldInspect(ag.ID, now) {
			continue
		}

		a.logger.Info("agent.watchdog.inspect",
			"id", ag.ID, "trigger", trigger,
			"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))

		a.wg.Go(func() { a.inspectAgent(ctx, ag, t, int(stall.Seconds()), int(total.Seconds())) })
	}
}

func (a *App) inspectAgent(ctx context.Context, ag *agent.Agent, t task.Task, stallSec, totalSec int) {
	ictx, cancel := context.WithTimeout(ctx, inspectorTimeout)
	defer cancel()

	verdict, err := agent.Inspect(ictx, agent.InspectInput{
		AgentID:   ag.ID,
		TaskTitle: t.Title,
		LogPath:   ag.GetLogPath(),
		StallSec:  stallSec,
		TotalSec:  totalSec,
	})
	if err != nil {
		a.logger.Warn("agent.watchdog.inspect.failed", "id", ag.ID, "err", err)
		return
	}

	a.logger.Info("agent.watchdog.verdict",
		"id", ag.ID, "stuck", verdict.Stuck,
		"recommendation", verdict.Recommendation, "reason", verdict.Reason)

	runtime.EventsEmit(a.ctx, events.AgentStuck(ag.ID), verdict)

	switch verdict.Recommendation {
	case "stop":
		if err := a.agents.StopAgent(ag.ID); err != nil {
			a.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
		}
		if ag.TaskID != "" {
			if _, err := a.tasks.Update(ag.TaskID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
				a.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
	case "escalate":
		if ag.TaskID != "" {
			if _, err := a.tasks.Update(ag.TaskID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
				a.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
	case "continue":
		// intentional no-op; debounce suppresses re-check
	}
}
