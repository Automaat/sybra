package agent

import (
	"context"
	"errors"
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
// tailing their log files. Records whose process is gone (or whose PID was
// reused by an unrelated process) are deleted. Returns the agents that
// were reattached. No-op when survival is disabled.
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
		// Phase 1 reattaches headless agents only. Interactive records are
		// handled by a later phase; leave them untouched here.
		if r.Mode != "headless" {
			continue
		}
		if !reattachAlive(r) {
			_ = reg.Delete(r.ID)
			m.logger.Info("agent.reattach.dead", "id", r.ID, "pid", r.PID, "task", r.TaskID)
			continue
		}

		a := agentFromRecord(r)
		if r.LogPath != "" {
			if evs, perr := ParseLogFile(r.LogPath, 0, r.Provider); perr == nil {
				a.outputBuffer = evs
			} else {
				m.logger.Warn("agent.reattach.rehydrate", "id", r.ID, "err", perr)
			}
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

		m.logger.Info("agent.reattach", "id", a.ID, "pid", a.PID, "task", a.TaskID, "events", len(a.outputBuffer))
		go m.reattachHeadless(ctx, a)
		m.emit(events.AgentState(a.ID), a)
		out = append(out, a)
	}
	return out
}

// reattachHeadless tails a reattached subprocess's log file until the
// process exits (or the app shuts down), then finalizes the agent through
// the same completion path as a freshly run one.
func (m *Manager) reattachHeadless(ctx context.Context, a *Agent) {
	procDone := make(chan struct{})
	go watchPID(ctx, a.GetPID(), procDone)

	exited := m.tailHeadlessFile(ctx, a, a.GetLogPath(), false, procDone)
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

// watchPID closes done when the process exits. On context cancel (app
// shutdown) it returns WITHOUT closing done, so the tailer takes its own
// ctx.Done path and leaves the agent running for the next reattach.
func watchPID(ctx context.Context, pid int, done chan struct{}) {
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
		}
	}
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
