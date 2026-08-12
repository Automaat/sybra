package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/events"
	providerpkg "github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/taskstatus"
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
var reattachPIDPoll atomic.Int64

func init() { reattachPIDPoll.Store(time.Second.Nanoseconds()) }

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
		if m.reapUnsupportedSurvivor(ctx, r, reg) {
			// Legacy interactive/per-turn-convo records can only exist for an
			// agent that was mid-flight when this steerable-headless-only binary
			// was deployed. Their runner no longer exists, so the record can
			// never be reattached — reap the orphaned process (which still holds
			// the worktree lock/FIFO) and drop the record instead of skipping it
			// silently and leaking it forever.
			continue
		}
		if !reattachAlive(r) { //nolint:contextcheck // liveness probe is context-free process inspection
			// A detached leader may be gone while an inherited provider/tool
			// child still occupies its process group. Confirm the entire original
			// group is gone before terminal reconciliation releases ownership.
			// If the PID was reused, the old process group cannot still reserve
			// that numeric ID; do not signal the unrelated replacement process.
			if !m.confirmDeadAttemptGroup(ctx, r) {
				continue
			}
			// Process gone. If it finished its work before vanishing,
			// finalize so the workflow advances instead of re-running it.
			// Otherwise (a genuine crash), bridge its captured session id to
			// the task so restart-stale recovery resumes instead of redoing.
			// Retain the record (skip delete) when the bridge fails so a later
			// startup retries it rather than losing the session permanently.
			completed := m.finalizeIfCompleted(r)
			if completed || m.persistDeadSession(r) {
				if !completed {
					a := fromRecord(r)
					m.completeAttempt(ctx, a, "lost")
				}
				_ = reg.Delete(r.ID)
				m.logger.Info("agent.reattach.dead", "id", r.ID, "pid", r.PID, "task", r.TaskID)
			} else {
				m.logger.Warn("agent.reattach.bridge-retry", "id", r.ID, "task", r.TaskID)
			}
			continue
		}

		decision := m.reattachDecide(r, time.Now().UTC())
		if decision.reason != "" {
			m.reapStaleSurvivor(ctx, r, reg, decision.reason)
			continue
		}
		intent := attemptIntentFromRecord(r)
		adoptedLease, err := m.adoptAttempt(ctx, r, intent)
		if err != nil {
			m.logger.Warn("agent.reattach.admission", "id", r.ID, "task", r.TaskID, "err", err)
			continue
		}
		if adoptedLease.ID != "" {
			r.AttemptIntentID = intent.IntentID
			r.AttemptTaskKey = intent.TaskID
			r.AttemptTaskGen = intent.TaskGeneration
			r.AttemptWorkGen = intent.WorktreeGeneration
			r.AttemptAccess = intent.Access
			r.AttemptLeaseID = adoptedLease.ID
			r.AttemptVersion = adoptedLease.Version
			if err := reg.Save(r); err != nil {
				m.logger.Warn("agent.reattach.lease-save", "id", r.ID, "task", r.TaskID, "err", err)
				// Fail closed: the observed process is still live, so releasing its
				// newly adopted lease here would permit a duplicate mutator. Keep
				// ownership occupied for reconciliation even though this instance
				// cannot safely expose the run.
				continue
			}
		}

		a := fromRecord(r)
		if decision.parkedStatus != "" {
			a.SetAdoptedParkedStatus(decision.parkedStatus)
			m.logger.Warn("agent.reattach.adopt-parked",
				"id", r.ID, "pid", r.PID, "task", r.TaskID, "task_status", decision.parkedStatus)
		}
		// Rehydrate the buffer and capture the exact byte offset consumed, so
		// the tailer resumes from there with no gap (a line appended between
		// rehydration and the tail's first read is not lost) and no
		// duplication.
		var startOffset int64
		if r.LogPath != "" {
			startOffset = m.rehydrateFromLog(a, r.LogPath)
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
		m.liveByClass[a.EffectiveRole().WorkloadClass()]++
		m.mu.Unlock()

		m.logger.Info("agent.reattach", "id", a.ID, "pid", a.PID, "task", a.TaskID, "events", len(a.Output()))
		if !m.notifyReattach(ctx, a) {
			continue
		}
		m.startAttemptHeartbeat(ctx, a)
		go m.reattachHeadless(ctx, a, startOffset, r.ProcStartedAt)
		m.emit(events.AgentState(a.ID), a)
		out = append(out, a)
	}
	return out
}

func (m *Manager) notifyReattach(ctx context.Context, a *Agent) bool {
	if m.onReattach == nil {
		return true
	}
	if err := m.onReattach(a); err != nil {
		m.logger.Error("agent.reattach.callback", "id", a.ID, "task", a.TaskID, "err", err)
		m.stopLocalAgent(a)
		m.markAgentDone(ctx, a)
		return false
	}
	return true
}

func (m *Manager) reapUnsupportedSurvivor(ctx context.Context, r Record, reg survivalRegistry) bool {
	if r.Mode == "headless" {
		return false
	}
	reason := "legacy_mode_" + r.Mode
	if !reattachAlive(r) { //nolint:contextcheck // liveness probe is context-free process inspection
		if m.confirmDeadAttemptGroup(ctx, r) {
			m.finalizeReapedSurvivor(ctx, r, reg, reason)
		}
		return true
	}
	m.reapStaleSurvivor(ctx, r, reg, reason)
	return true
}

func (m *Manager) confirmDeadAttemptGroup(ctx context.Context, r Record) bool {
	if recordPIDReused(ctx, r) || signalProcessGroupAndWait(r.PID, stopSIGINTGrace) {
		return true
	}
	m.logger.Error("agent.reattach.dead_group_unconfirmed", "id", r.ID, "pid", r.PID, "task", r.TaskID)
	return false
}

func recordPIDReused(ctx context.Context, r Record) bool {
	if r.PID <= 0 || r.ProcStartedAt == "" || !processAlive(r.PID) {
		return false
	}
	current := processStartString(ctx, r.PID)
	return current != "" && current != r.ProcStartedAt
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
	m.rehydrateFromLog(a, r.LogPath)
	if mt, ok := logActivityTime(r.LogPath); ok {
		a.SetLastEventAt(mt)
	}
	found, isError := resultBeforeOnlyForkOutput(a.Output())
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

// reattachHeadless tails a reattached subprocess's log file from
// startOffset until the process exits (or the app shuts down), then
// finalizes the agent through the same completion path as a freshly run
// one. A steerable headless run's stdin FIFO (see runHeadlessAttemptSurvive)
// is reopened first so a steer message sent after this reattach still
// reaches the child.
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

	m.reconcileReattachedHeadlessTerminalResult(a)
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
	if found, isError := resultBeforeOnlyForkOutput(outputs); !found {
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
	m.markAgentDone(ctx, a)
}

// reconcileReattachedHeadlessTerminalResult applies the terminal stdin
// boundary that was missed while the app was down. Error results deliberately
// retain queued steers for a retry; only a clean result may flush one into the
// still-live child.
func (m *Manager) reconcileReattachedHeadlessTerminalResult(a *Agent) {
	found, isError := resultBeforeOnlyForkOutput(a.Output())
	if !found {
		return
	}
	if isError {
		m.closeHeadlessSteerAfterError(a)
		return
	}
	m.drainOrCloseHeadlessSteer(a)
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
	t := time.NewTicker(time.Duration(reattachPIDPoll.Load()))
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
func (m *Manager) rehydrateFromLog(a *Agent, path string) int64 {
	return rehydrateFromLogWithArtifacts(a, path, a.TaskID, string(a.EffectiveRole()), m.artifacts)
}

func rehydrateFromLog(a *Agent, path string) int64 {
	return rehydrateFromLogWithArtifacts(a, path, "", "", nil)
}

func rehydrateFromLogWithArtifacts(a *Agent, path, taskID, producerRole string, store *artifact.Store) int64 {
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
		if isToolFailureDiagnosticEvent(ev.Type) {
			continue
		}
		ev = bindToolResultEvent(taskID, producerRole, store, ev)
		ev.Timestamp = time.Now().UTC()
		a.applyStreamEventState(ev)
		a.AppendOutput(ev)
		a.NoteSubagentCall(ev.parentToolUseID)
		// Restore per-run effort the same way the live stream loop accumulates
		// it (processHeadlessLine + checkTurnsGuardrail). Without this a run
		// that crosses an app restart — or completes while the app is down and
		// is finalized on reattach — records zero turns/tool calls.
		a.AddToolCalls(ev.ToolCalls)
		if ev.Type == "assistant" && ev.parentToolUseID == "" {
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
// Every event replayed by rehydrateFromLog is re-stamped at replay time, so
// without this a run finalized long after an app-downtime gap would report
// the full idle time as its duration. Returns false if the file cannot be
// statted (missing path, deleted log, etc.), in which case the caller falls
// back to the pre-fix behavior.
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
		// ReattachAll before any per-agent ctx exists yet (it decides whether
		// reattach happens at all).
		if cur := processStartString(context.Background(), r.PID); cur != "" && cur != r.ProcStartedAt {
			return false
		}
	}
	return true
}

// reattachMaxAge and reattachStuckAfter are the two halves of the
// liveness+progress reap policy: neither raw age nor a parked task status
// reaps a survivor on their own anymore — both defer to reattachProgressing,
// which is what actually decides whether the process is doing something.
const reattachMaxAge = 6 * time.Hour

// reattachStuckAfter bounds how long a live process's log file may go
// without a new line before the survivor is treated as stuck rather than
// progressing. Set well above normal tool-call latency (a slow build or test
// run legitimately produces no NDJSON lines for a while) so a healthy agent
// mid-tool-call is never misclassified as dead.
const reattachStuckAfter = 10 * time.Minute

// reattachToolCallGrace extends reattachStuckAfter for a survivor whose last
// logged event is a tool call with no result logged yet. A tool call emits no
// NDJSON for as long as the tool runs, so log silence on its own is not
// evidence that a live process is stuck — `go test ./...`, `npm ci` or a
// Playwright suite routinely outlive reattachStuckAfter, and SIGINTing one
// discards exactly the in-progress work this policy exists to protect. Still
// bounded, so a genuinely hung tool call is eventually reaped instead of
// pinning its worktree forever.
const reattachToolCallGrace = 2 * time.Hour

// reattachLogTailBytes bounds how much of a survivor's log is read back to
// find its last event. Agent logs reach tens of MB; only the tail matters.
const reattachLogTailBytes = 64 << 10

// reattachProgressing reports whether r's process has shown recent output
// activity, the only proxy available for "is this survivor actually doing
// something" (there is no *exec.Cmd to inspect, only the registry record and
// its log file). A record with no log to stat (missing path, deleted file)
// has no positive evidence of life beyond the OS-level PID check
// reattachAlive already performed, so it is treated as not progressing.
//
// A silent log past reattachStuckAfter is NOT taken as proof of a stuck
// process while the run is parked inside a tool call — see
// reattachAwaitingToolResult and reattachToolCallGrace.
func reattachProgressing(r Record, now time.Time) bool {
	mt, ok := logActivityTime(r.LogPath)
	if !ok {
		return false
	}
	silent := now.Sub(mt)
	if silent <= reattachStuckAfter {
		return true
	}
	return silent <= reattachToolCallGrace && reattachAwaitingToolResult(r)
}

// reattachAwaitingToolResult reports whether the last event r's process
// logged was a tool call whose result has not been logged yet — the one shape
// of silence that a healthy agent produces indefinitely. Providers spell it
// differently: codex/copilot/opencode emit a dedicated "tool_use" event,
// claude folds the tool call into the "assistant" event that carries it
// (ToolCalls > 0). A logged tool result (claude: "user"; others:
// "tool_result") or any later event means the tool already returned.
func reattachAwaitingToolResult(r Record) bool {
	ev, ok := lastLoggedStreamEvent(r.LogPath, r.Provider)
	if !ok {
		return false
	}
	switch ev.Type {
	case "tool_use":
		return true
	case "assistant":
		return ev.ToolCalls > 0
	default:
		return false
	}
}

// lastLoggedStreamEvent parses the last meaningful NDJSON event out of a
// survivor's log, reading only the tail. Returns false when the log is
// unreadable, empty, or holds nothing parseable.
func lastLoggedStreamEvent(path, providerName string) (StreamEvent, bool) {
	if path == "" {
		return StreamEvent{}, false
	}
	provider, err := lookupProvider(providerName)
	if err != nil {
		return StreamEvent{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return StreamEvent{}, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return StreamEvent{}, false
	}
	start := max(fi.Size()-reattachLogTailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, fi.Size()-start))
	if err != nil {
		return StreamEvent{}, false
	}
	lines := bytes.Split(data, []byte{'\n'})
	if start > 0 && len(lines) > 0 {
		// The tail window almost certainly cut the first line in half.
		lines = lines[1:]
	}
	for _, raw := range slices.Backward(lines) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		ev, perr := parseHeadlessEvent(line, provider)
		if perr != nil || ev.Type == "" || isToolFailureDiagnosticEvent(ev.Type) {
			continue
		}
		return ev, true
	}
	return StreamEvent{}, false
}

// reattachDecision is reattachDecide's verdict for one survivor record.
type reattachDecision struct {
	// reason, when non-empty, is why the survivor must be reaped instead of
	// adopted (logged and recorded by reapStaleSurvivor).
	reason string
	// parkedStatus is the task status a live, progressing survivor was
	// adopted over — empty for an ordinary adoption. Stamped onto the
	// reattached Agent so completion routing can keep the run's result while
	// suppressing workflow advancement (completion.Handler.OnComplete): the
	// workflow engine gates only on Workflow.State, so an implementation
	// workflow still in ExecWaiting would otherwise run its next step and
	// drag a deliberately parked (or done) task back into the pipeline.
	parkedStatus string
}

func (m *Manager) reattachDecide(r Record, now time.Time) reattachDecision {
	if strings.TrimSpace(r.TaskID) == "" {
		// The orchestrator brain is a deliberately taskless, long-lived
		// headless agent (see svc_orchestrator.go) — it must survive a
		// restart like any other live process, not be reaped as an orphan.
		if r.Role == RoleOrchestrator {
			return reattachDecision{}
		}
		return reattachDecision{reason: "no_task"}
	}
	if existsFn := m.taskExistsFn(); existsFn != nil && !existsFn(r.TaskID) {
		return reattachDecision{reason: "task_gone"}
	}

	progressing := reattachProgressing(r, now)

	var parked string
	if statusFn := m.taskStatusFn(); statusFn != nil {
		if status, ok := statusFn(r.TaskID); ok && ParksLiveAgent(status) {
			if !progressing {
				return reattachDecision{reason: "task_status_" + status}
			}
			// Healthy, progressing survivor whose task was parked while the
			// app was down (a monitor or human action) — adopt it instead of
			// killing uncommitted work, but carry the parked status forward so
			// its completion cannot re-drive the workflow.
			parked = status
		}
	}

	if !progressing && !r.StartedAt.IsZero() && now.Sub(r.StartedAt) > reattachMaxAge {
		return reattachDecision{reason: "deadline"}
	}
	return reattachDecision{parkedStatus: parked}
}

// ParksLiveAgent reports whether a task status means no live agent should be
// driving the task's workflow: a terminal outcome, or a queue/hold state a
// human or the monitor put it in. Shared by reattach (adoption policy) and
// completion routing (workflow-advancement suppression) so both read the same
// set.
func ParksLiveAgent(status string) bool {
	switch status {
	case string(taskstatus.Todo), string(taskstatus.New), string(taskstatus.HumanRequired),
		string(taskstatus.Blocked), string(taskstatus.Done), string(taskstatus.Cancelled):
		return true
	default:
		return false
	}
}

func (m *Manager) reapStaleSurvivor(ctx context.Context, r Record, reg survivalRegistry, reason string) {
	m.logger.Warn("agent.reattach.reap", "id", r.ID, "pid", r.PID, "task", r.TaskID, "reason", reason)
	if !signalPIDAndWait(r.PID, stopSIGINTGrace) {
		m.logger.Error("agent.reattach.reap_unconfirmed", "id", r.ID, "pid", r.PID, "task", r.TaskID)
		return
	}
	m.finalizeReapedSurvivor(ctx, r, reg, reason)
}

func (m *Manager) finalizeReapedSurvivor(ctx context.Context, r Record, reg survivalRegistry, reason string) {
	m.completeAttempt(ctx, fromRecord(r), "reaped_"+reason)
	if err := reg.Delete(r.ID); err != nil {
		m.logger.Warn("agent.reattach.reap.delete", "id", r.ID, "err", err)
	}
}
