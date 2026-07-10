package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	providerpkg "github.com/Automaat/sybra/internal/provider"
)

// errReattachedGone marks a reattached agent whose process exited without
// ever emitting a terminal result — treated as a crash so the completion
// handler stalls the workflow instead of advancing it on partial work.
var errReattachedGone = errors.New("agent: reattached process exited without result")

// errReattachedResultError marks a reattached agent that completed with an
// error result while the app was down — a real failure outcome (distinct
// from a crash with no result), so the workflow advances as failed.
var errReattachedResultError = errors.New("agent: reattached run completed with an error result")

// reattachPIDPoll is how often a reattached agent's PID is checked for
// liveness (there is no *exec.Cmd to Wait on). Var, not const, so tests
// can shorten it.
var reattachPIDPoll = time.Second

// ReattachAll rebuilds in-memory agents for subprocesses recorded in the
// registry that are still alive, and resumes streaming their output by
// tailing their log files. A record whose process is gone is finalized if
// its log shows the run completed (so a run that finished while the app was
// down is not redone), otherwise deleted. Records whose PID was reused by
// an unrelated process are treated as gone. Returns the reattached agents.
// No-op when survival is disabled.
//
// Call once at startup, before recovery's restart-stale sweep, so
// HasRunningAgentForTask sees the reattached agents and does not dispatch
// duplicates.
func (m *Manager) ReattachAll() []*Agent {
	return m.ReattachAllContext(m.ctx)
}

func (m *Manager) ReattachAllContext(ctx context.Context) []*Agent {
	reg := m.registry()
	if reg == nil {
		return nil
	}
	recs, err := reg.List()
	if err != nil {
		m.logger.Warn("agent.reattach.list", "err", err)
		return nil
	}

	var out []*Agent
	for i := range recs {
		r := recs[i]
		if r.Mode == "interactive" {
			// codex and copilot are per-turn conversational agents recreated
			// on restart; claude interactive reattaches to its live process.
			prov, providerErr := lookupProvider(r.Provider)
			if providerErr != nil {
				m.logger.Warn("agent.reattach.provider", "id", r.ID, "provider", r.Provider, "err", providerErr)
				continue
			}
			if prov.UsesPerTurnConvo() {
				if a := m.reattachPerTurnConvo(r, reg); a != nil { //nolint:contextcheck // reattach helpers rebuild from persisted manager state, not caller ctx
					out = append(out, a)
				}
				continue
			}
			if a := m.reattachInteractive(r, reg); a != nil { //nolint:contextcheck // reattach helpers rebuild from persisted manager state, not caller ctx
				out = append(out, a)
			}
			continue
		}
		if r.Mode != "headless" {
			continue
		}
		if !reattachAlive(r) { //nolint:contextcheck // liveness probe is context-free process inspection
			// Process gone. If it finished its work before vanishing,
			// finalize so the workflow advances instead of re-running it.
			// Otherwise (a genuine crash), bridge its captured session id to
			// the task so restart-stale recovery resumes instead of redoing.
			// Retain the record (skip delete) when the bridge fails so a later
			// startup retries it rather than losing the session permanently.
			if m.finalizeIfCompleted(r) || m.persistDeadSession(r) {
				_ = reg.Delete(r.ID)
				m.logger.Info("agent.reattach.dead", "id", r.ID, "pid", r.PID, "task", r.TaskID)
			} else {
				m.logger.Warn("agent.reattach.bridge-retry", "id", r.ID, "task", r.TaskID)
			}
			continue
		}

		if reason := m.reattachStaleReason(r, time.Now().UTC()); reason != "" {
			m.reapStaleSurvivor(r, reg, reason)
			continue
		}

		a := fromRecord(r)
		// Rehydrate the buffer and capture the exact byte offset consumed, so
		// the tailer resumes from there with no gap (a line appended between
		// rehydration and the tail's first read is not lost) and no
		// duplication.
		var startOffset int64
		if r.LogPath != "" {
			startOffset = rehydrateFromLog(a, r.LogPath)
		}

		ctx, cancel := context.WithCancel(ctx)
		a.cancel = cancel
		a.done = make(chan struct{})

		m.mu.Lock()
		if _, exists := m.agents[a.ID]; exists {
			m.mu.Unlock()
			cancel()
			continue
		}
		m.agents[a.ID] = a
		m.liveCount++
		if a.Provider != "" {
			m.liveByProvider[a.Provider]++
		}
		m.mu.Unlock()

		m.logger.Info("agent.reattach", "id", a.ID, "pid", a.PID, "task", a.TaskID, "events", len(a.Output()))
		go m.reattachHeadless(ctx, a, startOffset, r.ProcStartedAt)
		m.emit(events.AgentState(a.ID), a)
		out = append(out, a)
	}
	return out
}

// finalizeIfCompleted recovers a run whose process is gone but whose log
// shows a terminal result: it rebuilds a transient agent, rehydrates its
// buffer, and drives the normal completion callback so the workflow
// advances. Returns true if it finalized. A process gone without a terminal
// result is a genuine crash and is left for restart-stale / resume to
// handle (see persistDeadSession).
func (m *Manager) finalizeIfCompleted(r Record) bool {
	if r.LogPath == "" || r.Mode != "headless" {
		return false
	}
	a := fromRecord(r)
	rehydrateFromLog(a, r.LogPath)
	if mt, ok := logActivityTime(r.LogPath); ok {
		a.SetLastEventAt(mt)
	}
	found, isError := a.lastHeadlessResult()
	if !found {
		return false
	}
	if isError {
		outputs := a.Output()
		if err := resultStreamError(outputs); err != nil {
			a.SetExitErr(err)
			m.reportProviderHealthSignal(a, "", outputs)
		} else {
			a.SetExitErr(errReattachedResultError)
		}
	} else if m.reportCleanProviderHealthSignal(a, "", a.Output()) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.recovered-complete", "id", a.ID, "task", a.TaskID)
	m.fireComplete(m.ctx, a, a.GetExitErr() == nil)
	return true
}

// persistDeadSession bridges a crashed headless agent's captured session id
// from the registry into its task's AgentRun, so restart-stale recovery
// resumes the conversation via --resume instead of cold-restarting and
// redoing work. The session id otherwise lives only in the registry record
// (the completion callback that normally writes it to the AgentRun never
// ran on a hard crash) and would be lost when the record is deleted.
//
// Returns true when the record is safe to delete: nothing to bridge (no
// sink, session id, or task) or the bridge succeeded. Returns false only
// when the bridge was attempted and the sink failed, so the caller retains
// the record for a later retry rather than losing the session.
func (m *Manager) persistDeadSession(r Record) (done bool) {
	sink := m.sessionSinkFn()
	if sink == nil || r.SessionID == "" || r.TaskID == "" {
		return true
	}
	if err := sink(r.TaskID, r.ID, r.SessionID); err != nil {
		m.logger.Warn("agent.reattach.resume-session.failed",
			"id", r.ID, "task", r.TaskID, "session_id", r.SessionID, "err", err)
		return false
	}
	m.logger.Info("agent.reattach.resume-session", "id", r.ID, "task", r.TaskID, "session_id", r.SessionID)
	return true
}

// reattachInteractive rebuilds a live detached conversational agent: it
// rehydrates the convo buffer from the log, registers the agent, reopens its
// stdin FIFO, and resumes tailing. A dead record is deleted (interactive
// recovery is handled by the workflow's recoverStaleInteractive / chat gc).
// Returns the reattached agent, or nil when the process is gone or a
// duplicate already exists.
func (m *Manager) reattachInteractive(r Record, reg survivalRegistry) *Agent {
	if !reattachAlive(r) {
		_ = reg.Delete(r.ID)
		m.logger.Info("agent.reattach.dead", "id", r.ID, "pid", r.PID, "task", r.TaskID, "mode", "interactive")
		return nil
	}

	// Older records did not persist OneShot; retain the no-FIFO inference for
	// compatibility, but prefer the explicit flag for current records.
	oneShot := r.OneShot || r.StdinPath == ""

	a := fromRecord(r)
	a.oneShot = oneShot
	var startOffset int64
	if r.LogPath != "" {
		startOffset = rehydrateConvoFromLog(a, r.LogPath)
	}
	// Resume in the right state: Paused if the last turn finished (idle,
	// waiting for the user), Running if the agent was mid-generation at
	// restart — so a follow-up is queued, not injected mid-turn.
	a.SetState(convoResumeState(a.ConvoOutput()))

	ctx, cancel := context.WithCancel(m.ctx)
	a.cancel = cancel
	a.done = make(chan struct{})

	m.mu.Lock()
	if _, exists := m.agents[a.ID]; exists {
		m.mu.Unlock()
		cancel()
		return nil
	}
	m.agents[a.ID] = a
	m.liveCount++
	if a.Provider != "" {
		m.liveByProvider[a.Provider]++
	}
	m.mu.Unlock()

	m.logger.Info("agent.reattach", "id", a.ID, "pid", a.PID, "task", a.TaskID, "mode", "interactive", "oneshot", oneShot, "events", len(a.ConvoOutput()))
	go m.reattachConvo(ctx, a, startOffset, r.ProcStartedAt, oneShot)
	m.emit(events.AgentState(a.ID), a)
	return a
}

// convoResumeState infers a reattached conversational agent's state from
// its rehydrated buffer: Running when the most recent turn-shaping event is
// an assistant event (mid-generation), Paused when it is a result (idle
// between turns) or there is nothing yet.
func convoResumeState(evs []ConvoEvent) State {
	for i := range slices.Backward(evs) {
		switch evs[i].Type {
		case "result":
			return StatePaused
		case "assistant":
			return StateRunning
		}
	}
	return StatePaused
}

// reattachPerTurnConvo recreates a per-turn (codex/copilot) conversational
// agent after a restart.
// Codex convo has no persistent process between its independent per-turn
// invocations, so there is nothing to reattach to: rebuild the idle agent,
// rehydrate its chat from the log, and restart the loop in resume-wait mode
// (waiting for the next prompt). Liveness is not checked — the agent is
// recreated regardless. Returns nil only on a duplicate.
//
// r.Provider is authoritative even when a mid-run regateForTurn switch
// happened before the restart: regateForTurn persists the switched provider
// (and clears the old session id) via saveRegistry, so the record already
// reflects the peer the agent moved to, not the one it started on. The next
// prompt is dispatched via runPerTurnConversational(..., RunConfig{Dir:...},
// true), whose regate call reads the live current provider from a.Provider —
// not from this (mostly empty) RunConfig — so a stale/zero-value cfg.Provider
// here can never resurrect the pre-switch provider.
func (m *Manager) reattachPerTurnConvo(r Record, reg survivalRegistry) *Agent {
	// Per-turn recreate is unconditional (no live process to gate on), so guard
	// against resurrecting an agent for a task that was deleted while the app
	// was down — that would leak a zombie agent gcOrphanChats can't reap.
	if r.TaskID != "" {
		if exists := m.taskExistsFn(); exists != nil && !exists(r.TaskID) {
			_ = reg.Delete(r.ID)
			m.logger.Info("agent.reattach.skip", "id", r.ID, "task", r.TaskID, "reason", "task_gone")
			return nil
		}
	}

	a := fromRecord(r)
	if r.LogPath != "" {
		rehydratePerTurnConvoFromLog(a, r.LogPath)
	}
	if r.OneShot {
		if mt, ok := logActivityTime(r.LogPath); ok {
			a.SetLastEventAt(mt)
		}
		m.finalizePerTurnOneShot(r, a, reg)
		return nil
	}
	a.SetState(StatePaused)

	ctx, cancel := context.WithCancel(m.ctx)
	a.cancel = cancel
	a.done = make(chan struct{})
	a.setPromptChannel(make(chan string, 1))

	m.mu.Lock()
	if _, exists := m.agents[a.ID]; exists {
		m.mu.Unlock()
		cancel()
		return nil
	}
	m.agents[a.ID] = a
	m.liveCount++
	if a.Provider != "" {
		m.liveByProvider[a.Provider]++
	}
	m.mu.Unlock()

	m.logger.Info("agent.reattach", "id", a.ID, "task", a.TaskID, "mode", "interactive", "provider", a.Provider, "events", len(a.ConvoOutput()))
	// Resume idle: skip the first turn, wait for the next prompt. CWD/model
	// come from the rebuilt agent; the sandbox/approval choices are restored
	// from the record so a sandboxed chat stays sandboxed across restart.
	go m.runPerTurnConversational(ctx, a, perTurnReattachConfig(a), true)
	m.emit(events.AgentState(a.ID), a)
	return a
}

func perTurnReattachConfig(a *Agent) RunConfig {
	return RunConfig{
		Dir:                a.sessionCWD,
		RequirePermissions: a.requirePermissions,
		SandboxMode:        a.sandboxMode,
	}
}

func (m *Manager) finalizePerTurnOneShot(r Record, a *Agent, reg survivalRegistry) {
	found, isError := a.lastConvoResult()
	if !found {
		// The turn was interrupted mid-run (e.g. a Sybra restart) and never
		// produced a result — there is no completed outcome to report. Unlike
		// the headless case there is no process or session to reattach/resume,
		// so finalizing here as done or failed would either report false
		// success or burn the workflow step's retry budget on a turn that
		// never ran. Drop the stale record without firing completion and leave
		// the task's workflow step untouched: RestartStaleInProgress's
		// interactive-oneshot path re-dispatches a fresh turn instead.
		m.logger.Warn("agent.reattach.per-turn-oneshot.interrupted", "id", a.ID, "task", a.TaskID, "provider", a.Provider)
		_ = reg.Delete(r.ID)
		return
	}
	if isError {
		a.SetExitErr(errReattachedResultError)
		m.reportProviderHealthSignalConvo(a, "", a.ConvoOutput())
	} else if m.reportCleanProviderHealthSignalConvo(a, "", a.ConvoOutput()) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.per-turn-oneshot.done", "id", a.ID, "task", a.TaskID, "provider", a.Provider)
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(m.ctx, a, a.GetExitErr() == nil)
	_ = reg.Delete(r.ID)
}

// reattachHeadless tails a reattached subprocess's log file from
// startOffset until the process exits (or the app shuts down), then
// finalizes the agent through the same completion path as a freshly run
// one. A steerable headless run's stdin FIFO (see runHeadlessAttemptSurvive)
// is reopened first so a steer message sent after this reattach still
// reaches the child, mirroring reattachConvo.
func (m *Manager) reattachHeadless(ctx context.Context, a *Agent, startOffset int64, procStart string) {
	if sp := a.GetStdinPath(); sp != "" {
		if fifo, err := os.OpenFile(sp, os.O_RDWR, 0); err == nil {
			if installErr := a.convo.installStdinPipe(fifo); installErr != nil {
				_ = fifo.Close()
				m.logger.Warn("agent.reattach.fifo", "id", a.ID, "path", sp, "err", installErr)
			} else {
				// Steer transport restored — surface the capability to the UI.
				a.refreshCanSteer()
			}
		} else {
			m.logger.Warn("agent.reattach.fifo", "id", a.ID, "path", sp, "err", err)
		}
	}

	// If the run already emitted its terminal result while the app was down, it
	// is parked at a steer turn boundary that no live tailer ever processed —
	// rehydrateFromLog replays the result's stats but not the drain/close
	// boundary that handleHeadlessResult runs live. Replicate that boundary now,
	// before tailing: with nothing queued at reattach, drainOrCloseHeadlessSteer
	// closes stdin so the child sees EOF and exits like an unsteered one-shot,
	// instead of hanging (or accepting a post-reattach steer that would queue
	// forever with no further result to flush it).
	if found, _ := a.lastHeadlessResult(); found {
		m.drainOrCloseHeadlessSteer(a)
	}
	procDone := make(chan struct{})
	go watchPID(ctx, a.GetPID(), procStart, procDone)

	exited, _ := m.tailHeadlessFile(ctx, a, a.GetLogPath(), startOffset, procDone)
	if !exited {
		// App shutting down while the child is still alive: keep it running
		// and let the registry drive the next reattach.
		m.logger.Info("agent.reattach.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown")
		return
	}

	outputs := a.Output()
	if found, isError := lastHeadlessResultEvent(outputs); !found {
		a.SetExitErr(errReattachedGone)
	} else if isError {
		if err := resultStreamError(outputs); err != nil {
			a.SetExitErr(err)
			m.reportProviderHealthSignal(a, "", outputs)
		} else {
			a.SetExitErr(errReattachedResultError)
		}
	} else if m.reportCleanProviderHealthSignal(a, "", outputs) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(ctx, a, a.GetExitErr() == nil)
	m.markAgentDone(a)
}

// watchPID closes done when the process exits or is replaced by a PID-reuse
// (start time no longer matches). On context cancel (app shutdown) it
// returns WITHOUT closing done, so the tailer takes its own ctx.Done path
// and leaves the agent running for the next reattach.
func watchPID(ctx context.Context, pid int, procStart string, done chan struct{}) {
	if pid <= 0 {
		close(done)
		return
	}
	t := time.NewTicker(reattachPIDPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !processAlive(pid) {
				close(done)
				return
			}
			// Best-effort PID-reuse guard, matching reattachAlive: only act on
			// a definite mismatch. An empty current start time (ps unavailable)
			// must NOT be treated as a mismatch, or a still-running agent would
			// be wrongly marked gone.
			if procStart != "" {
				if cur := processStartString(ctx, pid); cur != "" && cur != procStart {
					close(done)
					return
				}
			}
		}
	}
}

// rehydrateFromLog replays an agent's NDJSON log into its output buffer
// (and cost/session stats) WITHOUT emitting events or running guardrails,
// and returns the byte offset just past the last complete line — the point
// from which live tailing should resume.
func rehydrateFromLog(a *Agent, path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return 0
	}

	provider, providerErr := lookupProvider(a.Provider)
	if providerErr != nil {
		a.SetError("provider", providerErr.Error())
		return 0
	}
	var offset int64
	start := 0
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		offset = int64(start)
		ev, perr := parseHeadlessEvent(line, provider)
		if perr != nil || ev.Type == "" {
			continue
		}
		ev.Timestamp = time.Now().UTC()
		a.AppendOutput(ev)
		// Restore per-run effort the same way the live stream loop accumulates
		// it (processHeadlessLine + checkTurnsGuardrail). Without this a run
		// that crosses an app restart — or completes while the app is down and
		// is finalized on reattach — records zero turns/tool calls.
		a.AddToolCalls(ev.ToolCalls)
		if ev.Type == "assistant" {
			a.IncTurnCount()
			// Copilot reports output tokens per assistant message (claude/codex
			// carry 0 here and total on the result event instead).
			if ev.OutputTokens > 0 {
				a.AddOutputTokens(ev.OutputTokens)
			}
		}
		if ev.Type == "result" {
			a.AddResultStats(ev.SessionID, ev.CostUSD, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens)
			a.AddCacheStats(ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
			a.AddPremiumRequests(ev.PremiumRequests)
		}
	}
	return offset
}

// logActivityTime returns the log file's mtime: the last time the (now-dead)
// process actually wrote to it, and the only faithful proxy for "when did
// this run actually finish" available on the dead-process recovery path.
// Every event replayed by rehydrateFromLog/rehydratePerTurnConvoFromLog is
// re-stamped at replay time, so without this a run finalized long after an
// app-downtime gap would report the full idle time as its duration. Returns
// false if the file cannot be statted (missing path, deleted log, etc.), in
// which case the caller falls back to the pre-fix behavior.
func logActivityTime(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// reattachAlive reports whether the recorded process is still the agent we
// spawned: alive, and (when we captured a start time) not a different
// process that reused the PID.
func reattachAlive(r Record) bool {
	if r.PID <= 0 || !processAlive(r.PID) {
		return false
	}
	if r.ProcStartedAt != "" {
		// context.Background(): reattachAlive is a free function called from
		// ReattachAll/reattachInteractive/reattachPerTurnConvo before any
		// per-agent ctx exists yet (it decides whether reattach happens at all).
		if cur := processStartString(context.Background(), r.PID); cur != "" && cur != r.ProcStartedAt {
			return false
		}
	}
	return true
}

const reattachMaxAge = 6 * time.Hour

func (m *Manager) reattachStaleReason(r Record, now time.Time) string {
	if strings.TrimSpace(r.TaskID) == "" {
		return "no_task"
	}
	if !r.StartedAt.IsZero() && now.Sub(r.StartedAt) > reattachMaxAge {
		return "deadline"
	}
	if statusFn := m.taskStatusFn(); statusFn != nil {
		if status, ok := statusFn(r.TaskID); ok && staleForLiveAgent(status) {
			return "task_status_" + status
		}
	}
	return ""
}

func staleForLiveAgent(status string) bool {
	switch status {
	case "todo", "new":
		return true
	default:
		return false
	}
}

func (m *Manager) reapStaleSurvivor(r Record, reg survivalRegistry, reason string) {
	m.logger.Warn("agent.reattach.reap", "id", r.ID, "pid", r.PID, "task", r.TaskID, "reason", reason)
	signalPID(r.PID, stopSIGINTGrace)
	if err := reg.Delete(r.ID); err != nil {
		m.logger.Warn("agent.reattach.reap.delete", "id", r.ID, "err", err)
	}
}
