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
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

const (
	TickInterval   = 30 * time.Second
	Debounce       = 5 * time.Minute
	InspectTimeout = 2 * time.Minute
)

// completedHangGrace bounds how long a headless agent may sit
// completed-but-alive (a terminal result observed, process not yet exited)
// before the watchdog force-stops it directly, bypassing the LLM judge. The
// runner's own post-result reaper (90s, internal/agent/runner_headless.go)
// already handles this for a live tailer, but that tailer only runs for a
// detached/reattached process — a non-detached run (agent.survive_restart:
// false) and a lost tailer goroutine leave nothing watching a finished run
// otherwise, so this is a hard backstop rather than the primary mechanism.
const completedHangGrace = 5 * time.Minute

const hardDeadlineMultiplier = 2

const (
	minHardWallClock = time.Hour
	minHardIdle      = 30 * time.Minute
)

func hardWallClockLimit(budget time.Duration) time.Duration {
	return max(hardDeadlineMultiplier*budget, minHardWallClock)
}

func hardIdleLimit(stallLim time.Duration) time.Duration {
	return max(hardDeadlineMultiplier*stallLim, minHardIdle)
}

func hardDeadlineBreach(ag *agent.Agent, stall, total, stallLim, budget time.Duration) string {
	switch {
	case total > hardWallClockLimit(budget):
		return "wall_clock"
	case stall > ag.EffectiveHangGrace(hardIdleLimit(stallLim)):
		return "idle"
	default:
		return ""
	}
}

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
	// stopCompletedAgent stops a headless agent that already produced a clean
	// terminal result, marking it completed-by-result first so the runner
	// finalizes it as a success rather than treating the kill signal as a
	// failure or stall. Used only by checkCompletedHang — every other
	// judge-driven verdict path uses the generic stopAgent.
	stopCompletedAgent func(string) error
	// nudgeAgent delivers a corrective steer to a live agent. Returns an error
	// for agents with no live transport (headless has no mid-stream stdin), in
	// which case applyVerdict degrades the nudge to an escalate.
	nudgeAgent func(agentID, text string) error
	// recordProviderSignal forwards a watchdog-detected provider signal through
	// the same agent-manager helper the runner uses, so the agent error kind and
	// provider health gate stay in sync across both paths.
	recordProviderSignal func(*agent.Agent, provider.Signal, string, time.Duration)
	// hasLiveHeadlessAgent reports whether a task has a registered live
	// headless agent. checkDwell uses it to skip escalating a task whose
	// headless run is mid-flight but hasn't touched the task file recently —
	// only the task-file timestamp is stale, not the agent itself. Dispatch
	// claims and conversational agents stay in dwell scope because the headless
	// stall watchdog cannot inspect them.
	hasLiveHeadlessAgent func(taskID string) bool
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
		agents:               agents,
		tasks:                tasks,
		logger:               logger,
		emit:                 emit,
		wg:                   wg,
		model:                cfg.Model,
		loopThreshold:        cfg.LoopThreshold,
		inspectAgent:         agent.Inspect,
		stopAgent:            agents.StopAgent,
		stopCompletedAgent:   agents.StopCompletedAgent,
		nudgeAgent:           agents.SendPromptToAgent,
		recordProviderSignal: agents.RecordProviderSignal,
		hasLiveHeadlessAgent: agents.HasLiveHeadlessAgentForTask,
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
		if ag.External {
			continue
		}
		state := ag.GetState()
		switch ag.Mode {
		case "headless":
			if state != agent.StateRunning {
				continue
			}
			if w.reapTaskAgentForStatus(ag) {
				continue
			}
			// A headless agent whose stream already ended in a successful terminal
			// result is logically complete; its process just has not exited yet (a
			// skill that spawns subagents can leave CC alive after the final
			// result). The runner's post-result guard usually finalizes it within
			// seconds, so never inspect or escalate such an agent — doing so flips
			// a finished run to human-required on a false stall (task c4a0fda0).
			// If it is still alive well past that grace, the runner's reaper isn't
			// covering it (e.g. non-detached run, lost tailer); hard-stop it
			// directly instead of leaving it to linger indefinitely.
			if ag.CompletedSuccessfully() {
				w.checkCompletedHang(ag, now)
				continue
			}
			w.inspectHeadless(ctx, s, now, ag)
		case "interactive":
			if state != agent.StateRunning && state != agent.StatePaused {
				continue
			}
			w.reapIdleInteractive(ag, now)
		default:
			continue
		}
	}
}

func (w *Watchdog) inspectHeadless(ctx context.Context, s *state, now time.Time, ag *agent.Agent) {
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

	if reason := hardDeadlineBreach(ag, stall, total, sl, budget); reason != "" {
		w.hardStop(ag, reason, stall, total)
		return
	}

	logPath := ag.GetLogPath()
	if logPath == "" {
		return
	}

	trigger := decideTrigger(ag.ToolLoopStreak(), w.loopThreshold, ag.ToolLoopAcknowledged(), stall, sl, total, budget)
	if trigger == "" {
		return
	}

	// A "stall" trigger on an agent that has never produced a single byte of
	// output since launch (LastEventAt untouched since StartedAt — see
	// AppendOutput/TouchLastEvent, the only writers) is not a mid-task hang;
	// the judge would have nothing to read from an empty log either. It is
	// the signature of a broken provider CLI invocation (auth hang, spawn
	// failure, wedged handshake) — #1913 saw the same claude process produce
	// zero NDJSON and zero stderr across three consecutive clean re-dispatches,
	// each burning the same-provider "watchdog hang" retry budget for nothing
	// before landing the task (and its umbrella parent) in human-required.
	// Route it through the provider-health signal path instead, exactly like
	// stopForRateLimit: this marks the provider unhealthy for its cooldown
	// window so the reschedule can fail over to a working peer, rather than
	// retrying the identical broken provider.
	if trigger == "stall" && ag.GetLastEventAt().Equal(ag.StartedAt) {
		w.handleZeroOutputStall(ag, stall, total)
		return
	}

	if !s.shouldInspect(ag.ID, now) {
		return
	}

	w.logger.Info("agent.watchdog.inspect",
		"id", ag.ID, "trigger", trigger,
		"loop_streak", ag.ToolLoopStreak(),
		"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))

	w.wg.Go(func() { w.inspect(ctx, ag, t, trigger, int(stall.Seconds()), int(total.Seconds())) })
}

func (w *Watchdog) reapTaskAgentForStatus(ag *agent.Agent) bool {
	if ag.TaskID == "" || w.tasks == nil {
		return false
	}
	t, err := w.tasks.Get(ag.TaskID)
	if err != nil {
		return false
	}
	if t.TaskType == task.TaskTypeChat || t.TaskType == task.TaskTypeUmbrella {
		return false
	}
	if !shouldReleaseTaskAgentForStatus(t.Status) {
		return false
	}
	w.logger.Warn("agent.watchdog.status_release",
		"id", ag.ID, "task_id", ag.TaskID, "status", t.Status)
	if err := w.stopForRelease(ag); err != nil {
		w.logger.Error("agent.watchdog.status_release.stop_failed", "id", ag.ID, "err", err)
	}
	return true
}

func (w *Watchdog) reapIdleInteractive(ag *agent.Agent, now time.Time) {
	if ag.TaskID == "" || w.tasks == nil {
		return
	}
	t, err := w.tasks.Get(ag.TaskID)
	if err != nil {
		return
	}
	if t.TaskType == task.TaskTypeChat || t.TaskType == task.TaskTypeUmbrella {
		return
	}
	if shouldReleaseTaskAgentForStatus(t.Status) {
		w.logger.Warn("agent.watchdog.status_release",
			"id", ag.ID, "task_id", ag.TaskID, "status", t.Status)
		if err := w.stopForRelease(ag); err != nil {
			w.logger.Error("agent.watchdog.status_release.stop_failed", "id", ag.ID, "err", err)
		}
		return
	}
	stall := now.Sub(ag.GetLastEventAt())
	total := now.Sub(ag.StartedAt)
	if reason := hardDeadlineBreach(ag, stall, total, stallLimit(t.Tags), sizeBudget(t.Tags)); reason != "" {
		w.hardStop(ag, reason, stall, total)
	}
}

func shouldReleaseTaskAgentForStatus(status task.Status) bool {
	return status == task.StatusHumanRequired || task.IsTerminalStatus(status)
}

func (w *Watchdog) stopForRelease(ag *agent.Agent) error {
	if ag.Mode == "headless" && ag.CompletedSuccessfully() {
		stop := w.stopCompletedAgent
		if stop == nil {
			stop = w.stopAgent
		}
		if stop == nil {
			return nil
		}
		return stop(ag.ID)
	}
	if w.stopAgent == nil {
		return nil
	}
	return w.stopAgent(ag.ID)
}

// checkCompletedHang force-stops a headless agent that finished its work (a
// non-error terminal result) but has sat idle beyond completedHangGrace
// without its process exiting. No judge inspection is involved — a finished
// run has nothing left to inspect, it just needs its orphaned process killed.
func (w *Watchdog) checkCompletedHang(ag *agent.Agent, now time.Time) {
	// EffectiveHangGrace extends the idle window while the CLI still reports
	// a live `run_in_background` task (e.g. npm ci), so this backstop doesn't
	// force-stop a process mid-write just because it produced no NDJSON
	// activity after its terminal result.
	if !ag.TerminalResultIdle(ag.EffectiveHangGrace(completedHangGrace)) {
		return
	}
	w.logger.Warn("agent.watchdog.completed_hang", "id", ag.ID,
		"idle_sec", int(now.Sub(ag.GetLastEventAt()).Seconds()),
		"background_tasks_pending", ag.HasBackgroundTasks())
	stop := w.stopCompletedAgent
	if stop == nil {
		stop = w.stopAgent
	}
	if stop == nil {
		w.logger.Error("agent.watchdog.completed_hang.no_stop_fn", "id", ag.ID)
		return
	}
	if err := stop(ag.ID); err != nil {
		w.logger.Error("agent.watchdog.completed_hang.stop_failed", "id", ag.ID, "err", err)
	}
}

func (w *Watchdog) hardStop(ag *agent.Agent, reason string, stall, total time.Duration) {
	w.logger.Warn("agent.watchdog.hard_deadline",
		"id", ag.ID, "task_id", ag.TaskID, "reason", reason,
		"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))
	if ag.TaskID != "" {
		if _, err := w.tasks.Update(ag.TaskID, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: task.Ptr("watchdog hang: " + reason + " deadline exceeded"),
		}); err != nil {
			w.logger.Error("agent.watchdog.hard_deadline.task.update", "task_id", ag.TaskID, "err", err)
		}
	}
	if err := w.stopAgent(ag.ID); err != nil {
		w.logger.Error("agent.watchdog.hard_deadline.stop.failed", "id", ag.ID, "err", err)
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
		"recommendation", verdict.Recommendation, "reason", verdict.Reason,
		"reason_kind", verdict.ReasonKind)

	w.emit(events.AgentStuck(ag.ID), verdict)
	w.applyVerdict(ag, trigger, verdict)

	// Acknowledge a loop-triggered inspection that left the agent running, so the
	// same unchanged signature does not re-trigger every debounce window. Skip
	// the ack on a "stop" verdict: if the stop fails the agent keeps looping, and
	// acking would suppress the next inspection that could retry the stop. A
	// genuinely new loop (different signature) re-arms the trigger automatically.
	if trigger == "loop" && verdict.Recommendation != "stop" {
		ag.AckToolLoop()
	}
}

func (w *Watchdog) applyVerdict(ag *agent.Agent, trigger string, verdict agent.InspectorVerdict) {
	switch verdict.Recommendation {
	case "stop":
		if verdict.ReasonKind == "rate_limit" {
			w.stopForRateLimit(ag, trigger, verdict)
			return
		}
		// Set the task state before stopping so the completion callback sees the
		// intended recovery path. rate_limit is already handled above regardless
		// of trigger. Of what remains: a stall stop is a retryable hang; the
		// workflow engine consumes the marker from ResumeStalled. A loop or
		// budget stop whose judge reason is "generic_stall" (a benign
		// command-repetition or long-running-verify-poll flake, not
		// reward-hacking) gets the same retryable treatment — see #1456.
		// Reward-hacking loops (and any other non-rate_limit reason_kind on a
		// budget trigger) remain immediate human-required escalations.
		if ag.TaskID != "" {
			reason := "watchdog stop"
			if verdict.Reason != "" {
				reason = "watchdog: " + verdict.Reason
			}
			status := task.StatusHumanRequired
			if trigger == "stall" || ((trigger == "loop" || trigger == "budget") && verdict.ReasonKind == "generic_stall") {
				status = task.StatusInProgress
				reason = "watchdog hang"
				if verdict.Reason != "" {
					reason = "watchdog hang: " + verdict.Reason
				}
			}
			if _, err := w.tasks.Update(ag.TaskID, task.Update{
				Status:       task.Ptr(status),
				StatusReason: task.Ptr(reason),
			}); err != nil {
				w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
			}
		}
		if err := w.stopAgent(ag.ID); err != nil {
			w.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
		}
	case "nudge":
		// Steer a drifting-but-recoverable agent. An agent with a live transport
		// (interactive/conversational) is nudged in place. A headless agent has
		// no mid-stream channel, so fall back to the resume path: persist the
		// steer on the task and stop the looping run, so the recovery loop
		// re-dispatches (resumes) it with the correction prepended.
		steer := verdict.Nudge
		if steer == "" {
			steer = verdict.Reason
		}
		if steer == "" {
			steer = "you appear to be repeating the same action; stop and reconsider your approach"
		}
		if w.nudgeAgent != nil {
			if err := w.nudgeAgent(ag.ID, supervisorNudgePrefix+steer); err == nil {
				w.logger.Info("agent.watchdog.nudge", "id", ag.ID, "transport", "live", "steer", steer)
				return
			} else {
				w.logger.Info("agent.watchdog.nudge.no_live_transport", "id", ag.ID, "err", err)
			}
		}
		w.headlessNudge(ag, steer)
	case "escalate":
		// Ambiguous verdicts are advisory only. Flipping the task to
		// human-required while the agent keeps running leaves task status
		// inconsistent and can strand successfully completing work.
	case "continue":
		// intentional no-op; debounce suppresses re-check
	}
}

// stopForRateLimit handles a "stop" verdict whose ReasonKind is "rate_limit":
// a provider rate/quota limit, not a genuine hang or reward-hacking loop.
// This reuses the same recovery machinery already trusted for a structured
// 429 (internal/agent/runner_headless_retry.go): mark the agent's error kind
// so the completion handler's isRateLimitedRun check recognizes the stop as
// rate-limited and calls RescheduleRateLimitedAgent instead of stranding the
// task in human-required, and report the signal to the provider health gate
// so it applies the same cooldown/failover as the clean-429 case. The task is
// left in-progress — human-required is reserved for genuine reward-hacking
// loops per #1310's scoping.
func (w *Watchdog) stopForRateLimit(ag *agent.Agent, trigger string, verdict agent.InspectorVerdict) {
	reason := "watchdog: rate limit"
	if verdict.Reason != "" {
		reason = "watchdog: rate limit: " + verdict.Reason
	}
	if ag.TaskID == "" {
		w.logger.Warn("agent.watchdog.rate_limit.untracked",
			"id", ag.ID, "trigger", trigger, "provider", ag.Provider, "reason", verdict.Reason)
	} else if _, err := w.tasks.Update(ag.TaskID, task.Update{
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
	}
	if w.recordProviderSignal != nil {
		w.recordProviderSignal(ag, provider.SignalRateLimit, reason, 0)
	}
	if err := w.stopAgent(ag.ID); err != nil {
		w.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
	}
	w.logger.Info("agent.watchdog.rate_limit.stop",
		"id", ag.ID, "task_id", ag.TaskID, "trigger", trigger, "provider", ag.Provider, "reason", verdict.Reason)
}

// zeroOutputReason is the provider-health detail recorded for a zero-output
// startup hang (see inspectHeadless). Kept distinct from the generic
// "rate_limited" reason so provider health status/logs can tell the two
// apart even though both share the SignalRateLimit health-gate bucket.
const zeroOutputReason = "zero output before startup timeout"

// handleZeroOutputStall handles a "stall" trigger on a headless agent that
// never produced any output at all. This reuses stopForRateLimit's recovery
// machinery: mark the task retryable (not human-required), report a
// provider-health signal so the health gate parks this provider for its
// cooldown and the next dispatch can fail over to a healthy peer, and stop
// the hung process. Reusing the "rate_limit" signal/error-kind is what makes
// the completion handler's isRateLimitedRun check reschedule the run
// immediately (RescheduleRateLimitedAgent) instead of leaving it for the
// same-provider "watchdog hang" retry budget that #1913 exhausted for
// nothing.
func (w *Watchdog) handleZeroOutputStall(ag *agent.Agent, stall, total time.Duration) {
	w.logger.Warn("agent.watchdog.zero_output_stall",
		"id", ag.ID, "task_id", ag.TaskID, "provider", ag.Provider,
		"stall_sec", int(stall.Seconds()), "total_sec", int(total.Seconds()))
	reason := "watchdog: rate limit: " + zeroOutputReason
	if ag.TaskID == "" {
		w.logger.Warn("agent.watchdog.zero_output_stall.untracked", "id", ag.ID, "provider", ag.Provider)
	} else if _, err := w.tasks.Update(ag.TaskID, task.Update{
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr(reason),
	}); err != nil {
		w.logger.Error("agent.watchdog.task.update", "task_id", ag.TaskID, "err", err)
	}
	if w.recordProviderSignal != nil {
		w.recordProviderSignal(ag, provider.SignalRateLimit, zeroOutputReason, 0)
	}
	if err := w.stopAgent(ag.ID); err != nil {
		w.logger.Error("agent.watchdog.stop.failed", "id", ag.ID, "err", err)
	}
}

// supervisorNudgePrefix tags a watchdog steer delivered to a live agent so the
// agent can tell it apart from a user message.
const supervisorNudgePrefix = "⚠️ Supervisor: "

// headlessNudge delivers a course-correction to a headless agent, which has no
// mid-stream channel. It persists the steer on the task and stops the looping
// run; the run's session is captured for --resume, and the recovery loop
// re-dispatches the still-in-progress task with the steer prepended to the
// prompt. The task is deliberately left in-progress (not human-required) so
// recovery resumes it rather than parking it for a human.
func (w *Watchdog) headlessNudge(ag *agent.Agent, steer string) {
	if ag.TaskID != "" {
		if _, err := w.tasks.Update(ag.TaskID, task.Update{SupervisorSteer: task.Ptr(steer)}); err != nil {
			w.logger.Error("agent.watchdog.nudge.steer", "task_id", ag.TaskID, "err", err)
		}
	}
	if err := w.stopAgent(ag.ID); err != nil {
		w.logger.Error("agent.watchdog.nudge.stop", "id", ag.ID, "err", err)
		return
	}
	w.logger.Info("agent.watchdog.nudge", "id", ag.ID, "transport", "headless-resume", "steer", steer)
}
