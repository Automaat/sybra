package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
)

// errReattachedGone marks a reattached agent whose process exited without
// ever emitting a terminal result — treated as a crash so the completion
// handler stalls the workflow instead of advancing it on partial work.
var errReattachedGone = errors.New("agent: reattached process exited without result")

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
			if a := m.reattachInteractive(r, reg); a != nil {
				out = append(out, a)
			}
			continue
		}
		if r.Mode != "headless" {
			continue
		}
		if !reattachAlive(r) {
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

		a := agentFromRecord(r)
		// Rehydrate the buffer and capture the exact byte offset consumed, so
		// the tailer resumes from there with no gap (a line appended between
		// rehydration and the tail's first read is not lost) and no
		// duplication.
		var startOffset int64
		if r.LogPath != "" {
			startOffset = rehydrateFromLog(a, r.LogPath)
		}

		ctx, cancel := context.WithCancel(m.ctx)
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
	a := agentFromRecord(r)
	rehydrateFromLog(a, r.LogPath)
	if !a.hasTerminalResult() {
		return false
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.recovered-complete", "id", a.ID, "task", a.TaskID)
	m.recordCompletion(a, true)
	if m.onComplete != nil {
		m.onComplete(a)
	}
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
func (m *Manager) reattachInteractive(r Record, reg *registryStore) *Agent {
	if !reattachAlive(r) {
		_ = reg.Delete(r.ID)
		m.logger.Info("agent.reattach.dead", "id", r.ID, "pid", r.PID, "task", r.TaskID, "mode", "interactive")
		return nil
	}

	a := agentFromRecord(r)
	var startOffset int64
	if r.LogPath != "" {
		startOffset = rehydrateConvoFromLog(a, r.LogPath)
	}
	// Interactive agents sit idle (Paused) between user turns; reattach in
	// that state. processConvoLine flips to Running when a follow-up fires.
	a.SetState(StatePaused)

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
	m.mu.Unlock()

	m.logger.Info("agent.reattach", "id", a.ID, "pid", a.PID, "task", a.TaskID, "mode", "interactive", "events", len(a.ConvoOutput()))
	go m.reattachConvo(ctx, a, startOffset, r.ProcStartedAt)
	m.emit(events.AgentState(a.ID), a)
	return a
}

// reattachHeadless tails a reattached subprocess's log file from
// startOffset until the process exits (or the app shuts down), then
// finalizes the agent through the same completion path as a freshly run
// one.
func (m *Manager) reattachHeadless(ctx context.Context, a *Agent, startOffset int64, procStart string) {
	procDone := make(chan struct{})
	go watchPID(ctx, a.GetPID(), procStart, procDone)

	exited, _ := m.tailHeadlessFile(ctx, a, a.GetLogPath(), startOffset, procDone)
	if !exited {
		// App shutting down while the child is still alive: keep it running
		// and let the registry drive the next reattach.
		m.logger.Info("agent.reattach.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown")
		return
	}

	if !a.hasTerminalResult() {
		a.SetExitErr(errReattachedGone)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, a.GetExitErr() == nil)
	if m.onComplete != nil {
		m.onComplete(a)
	}
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
				if cur := processStartString(pid); cur != "" && cur != procStart {
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

	isCodex := normalizeProvider(a.Provider) == "codex"
	var offset int64
	start := 0
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		offset = int64(start)
		ev, perr := parseHeadlessEvent(line, isCodex)
		if perr != nil || ev.Type == "" {
			continue
		}
		ev.Timestamp = time.Now().UTC()
		a.AppendOutput(ev)
		if ev.Type == "result" {
			a.AddResultStats(ev.SessionID, ev.CostUSD, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens)
			a.AddCacheStats(ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
		}
	}
	return offset
}

// reattachAlive reports whether the recorded process is still the agent we
// spawned: alive, and (when we captured a start time) not a different
// process that reused the PID.
func reattachAlive(r Record) bool {
	if r.PID <= 0 || !processAlive(r.PID) {
		return false
	}
	if r.ProcStartedAt != "" {
		if cur := processStartString(r.PID); cur != "" && cur != r.ProcStartedAt {
			return false
		}
	}
	return true
}

func agentFromRecord(r Record) *Agent {
	return &Agent{
		ID:          r.ID,
		TaskID:      r.TaskID,
		Name:        r.Name,
		Mode:        r.Mode,
		Provider:    r.Provider,
		Model:       r.Model,
		PID:         r.PID,
		SessionID:   r.SessionID,
		LogPath:     r.LogPath,
		sessionCWD:  r.CWD,
		StartedAt:   r.StartedAt,
		LastEventAt: time.Now().UTC(),
		State:       StateRunning,
		MaxTurns:    r.MaxTurns,
		stdinPath:   r.StdinPath,
		detached:    true,
	}
}

// processStartString returns the OS-reported start time of a process as an
// opaque string, used only for equality comparison to detect PID reuse.
// Best-effort: an empty result disables the guard for that record.
func processStartString(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
