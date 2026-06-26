package watchdog

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/task"
)

const (
	TickInterval   = 30 * time.Second
	Debounce       = 5 * time.Minute
	InspectTimeout = 2 * time.Minute
)

// stallLimit returns the max event-gap before triggering inspection.
func stallLimit(tags []string) time.Duration {
	switch {
	case slices.Contains(tags, "large"):
		return 45 * time.Minute
	case slices.Contains(tags, "small"):
		return 10 * time.Minute
	default: // medium or unset
		return 15 * time.Minute
	}
}

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

// Watchdog monitors headless agents for stalls, budget overruns, and tool-call
// loops, inspecting suspect agents with a cheap judge and stopping, nudging, or
// escalating based on the verdict.
type Watchdog struct {
	agents *agent.Manager
	tasks  *task.Manager
	logger *slog.Logger
	emit   func(string, any)
	wg     *sync.WaitGroup

	// model is the judge model passed to the inspector (e.g. a cheap "haiku").
	model string
	// loopThreshold is the consecutive identical tool-call count that flags an
	// agent as looping. Zero disables the real-time loop trigger.
	loopThreshold int

	inspectAgent func(context.Context, *slog.Logger, agent.InspectInput) (agent.InspectorVerdict, error)
	stopAgent    func(string) error
	// nudgeAgent delivers a corrective steer to a live agent. Returns an error
	// for agents with no live transport (headless has no mid-stream stdin), in
	// which case applyVerdict degrades the nudge to an escalate.
	nudgeAgent func(agentID, text string) error
}

// New creates a Watchdog. cfg.Model selects the cheap judge model and
// cfg.LoopThreshold tunes the real-time loop trigger.
func New(
	agents *agent.Manager,
	tasks *task.Manager,
	logger *slog.Logger,
	emit func(string, any),
	wg *sync.WaitGroup,
	cfg config.WatchdogConfig,
) *Watchdog {
	return &Watchdog{
		agents:        agents,
		tasks:         tasks,
		logger:        logger,
		emit:          emit,
		wg:            wg,
		model:         cfg.Model,
		loopThreshold: cfg.LoopThreshold,
		inspectAgent:  agent.Inspect,
		stopAgent:     agents.StopAgent,
		nudgeAgent:    agents.SendPromptToAgent,
	}
}

// Run blocks until ctx is cancelled, ticking every TickInterval.
func (w *Watchdog) Run(ctx context.Context) {
	s := newState()
	ticker := time.NewTicker(TickInterval)
	dwellTicker := time.NewTicker(DwellTickInterval)
	defer ticker.Stop()
	defer dwellTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.tick(ctx, s, now)
		case now := <-dwellTicker.C:
			w.checkDwell(now)
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
		var budget, sl time.Duration
		if err == nil {
			budget = sizeBudget(t.Tags)
			sl = stallLimit(t.Tags)
		} else {
			budget = sizeBudget(nil)
			sl = stallLimit(nil)
		}

		trigger := decideTrigger(ag.ToolLoopStreak(), w.loopThreshold, ag.ToolLoopAcknowledged(), stall, sl, total, budget)
		if trigger == "" {
			continue
		}
		if !s.shouldInspect(ag.ID, now) {
			continue
		}

		w.logger.Info("agent.watchdog.inspect",
			"id", ag.ID, "trigger", trigger,
			"loop_streak", ag.ToolLoopStreak(),
			"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))

		w.wg.Go(func() { w.inspect(ctx, ag, t, trigger, int(stall.Seconds()), int(total.Seconds())) })
	}
}

// decideTrigger selects the inspection trigger for an agent, or "" when none
// fires. The loop trigger takes precedence — an actively looping agent never
// stalls, so catching the loop early is the whole point — but only while the
// loop is unacknowledged: once a loop has been inspected and cleared, loopAcked
// suppresses it so the same unchanged loop neither re-storms the judge nor
// masks the stall trigger for a now-frozen agent. A non-positive loopThreshold
// disables loop detection.
func decideTrigger(loopStreak, loopThreshold int, loopAcked bool, stall, stallLim, total, budget time.Duration) string {
	switch {
	case loopThreshold > 0 && loopStreak >= loopThreshold && !loopAcked:
		return "loop"
	case stall > stallLim:
		return "stall"
	case total > budget:
		return "budget"
	default:
		return ""
	}
}

func (w *Watchdog) inspect(ctx context.Context, ag *agent.Agent, t task.Task, trigger string, stallSec, totalSec int) {
	ictx, cancel := context.WithTimeout(ctx, InspectTimeout)
	defer cancel()

	verdict, err := w.inspectAgent(ictx, w.logger, agent.InspectInput{
		AgentID:   ag.ID,
		TaskTitle: t.Title,
		LogPath:   ag.GetLogPath(),
		StallSec:  stallSec,
		TotalSec:  totalSec,
		Trigger:   trigger,
		Model:     w.model,
	})
	if err != nil {
		w.logger.Warn("agent.watchdog.inspect.failed", "id", ag.ID, "err", err)
		return
	}

	w.logger.Info("agent.watchdog.verdict",
		"id", ag.ID, "stuck", verdict.Stuck,
		"recommendation", verdict.Recommendation, "reason", verdict.Reason)

	w.emit(events.AgentStuck(ag.ID), verdict)
	w.applyVerdict(ag, verdict)

	// Acknowledge a loop-triggered inspection that left the agent running, so the
	// same unchanged signature does not re-trigger every debounce window. Skip
	// the ack on a "stop" verdict: if the stop fails the agent keeps looping, and
	// acking would suppress the next inspection that could retry the stop. A
	// genuinely new loop (different signature) re-arms the trigger automatically.
	if trigger == "loop" && verdict.Recommendation != "stop" {
		ag.AckToolLoop()
	}
}

func (w *Watchdog) applyVerdict(ag *agent.Agent, verdict agent.InspectorVerdict) {
	switch verdict.Recommendation {
	case "stop":
		// Set human-required before stopping so the AdvanceStep callback
		// (fired via onComplete after the agent exits) sees the escalated
		// status and the workflow stops instead of advancing to the next step.
		if ag.TaskID != "" {
			reason := "watchdog stop"
			if verdict.Reason != "" {
				reason = "watchdog: " + verdict.Reason
			}
			if _, err := w.tasks.Update(ag.TaskID, task.Update{
				Status:       task.Ptr(task.StatusHumanRequired),
				StatusReason: task.Ptr(reason),
			}); err != nil {
				w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
		if err := w.stopAgent(ag.ID); err != nil {
			w.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
		}
	case "nudge":
		// Deliver a corrective steer to a live agent and leave it running. Only
		// agents with a live transport (interactive/conversational) can be
		// nudged mid-stream; a headless agent has no stdin, so SendPromptToAgent
		// errors and the nudge degrades to an advisory escalate (leave running,
		// debounce suppresses re-check). Headless nudge via stop+resume is
		// tracked separately.
		steer := verdict.Nudge
		if steer == "" {
			steer = verdict.Reason
		}
		if w.nudgeAgent == nil {
			w.logger.Warn("agent.watchdog.nudge.unwired", "id", ag.ID)
			return
		}
		if err := w.nudgeAgent(ag.ID, "⚠️ Supervisor: "+steer); err != nil {
			w.logger.Warn("agent.watchdog.nudge.degraded",
				"id", ag.ID, "err", err, "steer", steer)
			return
		}
		w.logger.Info("agent.watchdog.nudge", "id", ag.ID, "steer", steer)
	case "escalate":
		// Ambiguous verdicts are advisory only. Flipping the task to
		// human-required while the agent keeps running leaves task status
		// inconsistent and can strand successfully completing work.
	case "continue":
		// intentional no-op; debounce suppresses re-check
	}
}
