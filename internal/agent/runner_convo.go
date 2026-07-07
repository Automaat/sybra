package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	providerpkg "github.com/Automaat/sybra/internal/provider"
)

// claudeEventToConvoEvent converts a shared ClaudeEvent into a ConvoEvent for
// conversational mode. Tool result content is truncated to 2000 chars.
func claudeEventToConvoEvent(e ClaudeEvent) ConvoEvent {
	ev := ConvoEvent{
		Type:      e.Type,
		Subtype:   e.Subtype,
		SessionID: e.SessionID,
		Timestamp: time.Now().UTC(),
		Raw:       e.Raw,
	}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Text = e.Message.Text
			ev.ToolUses = e.Message.ToolUses
		}
	case "user":
		if e.Message != nil {
			results := make([]ToolResultBlock, len(e.Message.ToolResults))
			copy(results, e.Message.ToolResults)
			for i := range results {
				if len(results[i].Content) > 2000 {
					results[i].Content = results[i].Content[:2000] + "..."
				}
			}
			ev.ToolResults = results
		}
	case "result":
		if e.Result != nil {
			ev.Text = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.CostUSD = e.Result.CostUSD
			ev.InputTokens = e.Result.InputTokens
			ev.OutputTokens = e.Result.OutputTokens
			ev.CacheCreationInputTokens = e.Result.CacheCreationInputTokens
			ev.CacheReadInputTokens = e.Result.CacheReadInputTokens
			ev.ReasoningTokens = e.Result.ReasoningTokens
		}
	}
	return ev
}

// convoEmitInterval caps event emission rate for conversational agents.
const convoEmitInterval = 50 * time.Millisecond

func (m *Manager) buildConvoArgs(a *Agent, cfg RunConfig) []string {
	// Interactive session: messages arrive on stdin as stream-json.
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	return append(args, m.convoCommonArgs(a, cfg)...)
}

// buildOneShotConvoArgs builds args for a single detached conversational
// turn: the prompt is passed as an argument (no stdin), so the process needs
// no FIFO, reads nothing from stdin, runs the one turn, and exits — which
// makes it survive a restart while still emitting the same stream-json the
// convo parser consumes. (A regular-file stdin does NOT work: claude only
// reads stream-json from a pipe, not a regular file.)
func (m *Manager) buildOneShotConvoArgs(a *Agent, cfg RunConfig) []string {
	args := []string{
		"-p", cfg.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	return append(args, m.convoCommonArgs(a, cfg)...)
}

// convoCommonArgs returns the resume/permission/model/approval-hook flags
// shared by the interactive and one-shot conversational invocations.
func (m *Manager) convoCommonArgs(a *Agent, cfg RunConfig) []string {
	args := make([]string, 0, 8)
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	args = append(args, effortArgs(a.ReasoningEffort)...)
	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else if !cfg.RequirePermissions && cfg.PermissionMode == "" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	needsApproval := cfg.RequirePermissions || cfg.PermissionMode != ""
	if hookSettings := buildClaudeHookSettings(m.approvalAddr, needsApproval); hookSettings != "" {
		args = append(args, "--settings", hookSettings)
	}
	return args
}

type claudeHookSettings struct {
	Hooks map[string][]claudeHookEntry `json:"hooks"`
}

type claudeHookEntry struct {
	Matcher string             `json:"matcher"`
	Hooks   []claudeHookAction `json:"hooks"`
}

type claudeHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Timeout int    `json:"timeout"`
}

func buildClaudeHookSettings(approvalAddr string, needsApproval bool) string {
	var actions []claudeHookAction
	if bin, ok := resolveKlaudiushHookBin(); ok {
		actions = append(actions, claudeHookAction{
			Type:    "command",
			Command: bin + " --hook-type PreToolUse",
			Timeout: 30,
		})
	}

	// Only wire the approval hook for agents that actually need permission checks.
	// Agents with --dangerously-skip-permissions still get klaudiush validation,
	// but should not block on Sybra's human approval server.
	if approvalAddr != "" && needsApproval {
		actions = append(actions, claudeHookAction{
			Type:    "http",
			URL:     fmt.Sprintf("http://%s/hooks/pre-tool-use", approvalAddr),
			Timeout: 300,
		})
	}
	if len(actions) == 0 {
		return ""
	}
	settings := claudeHookSettings{
		Hooks: map[string][]claudeHookEntry{
			"PreToolUse": {{
				Matcher: "",
				Hooks:   actions,
			}},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *Manager) startConvoProcess(ctx context.Context, a *Agent, cfg RunConfig) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	args := m.buildConvoArgs(a, cfg)
	cmd := newProviderCmd(ctx, &cfg, false, "claude", args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	a.convo.replaceStdinPipe(stdinPipe)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.convo.closeStdinPipe()
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderrBuf := new(bytes.Buffer)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		a.convo.closeStdinPipe()
		return nil, nil, nil, fmt.Errorf("start claude: %w", err)
	}
	return cmd, stdout, stderrBuf, nil
}

func (m *Manager) runConversational(ctx context.Context, a *Agent, cfg RunConfig) {
	var outFile *os.File
	defer func() {
		if outFile != nil {
			_ = outFile.Close()
		}
	}()

	// tailOffset tracks the survival file tailer's position across retries.
	var tailOffset int64

	if earlyReturn := m.runRetryLoop(ctx, a, "convo", func(int) (bool, error) {
		return m.runConvoAttempt(ctx, a, cfg, &outFile, &tailOffset)
	}); earlyReturn {
		return
	}

	// A pending handoff (SendMessage/regateBeforeClaudeTurn regated this
	// agent's next turn onto a healthy per-turn peer and tore down the idle
	// Claude process to force this exit) takes over instead of finalizing:
	// the same *Agent hands off to the per-turn runner on the new provider.
	if handoffCfg, prompt, ok := a.ConsumePendingHandoff(); ok {
		m.completeConvoHandoff(ctx, a, outFile, handoffCfg, prompt)
		outFile = nil
		return
	}

	m.finalizeRun(ctx, a, "agent.convo.done")
}

// beginConvoHandoff records a regated provider switch on a persistent Claude
// interactive agent and tears down its (idle) process so runConversational's
// goroutine exits and completeConvoHandoff can take over. Called from
// SendMessage when regateBeforeClaudeTurn finds Claude capped and a healthy
// per-turn-capable peer available.
func (m *Manager) beginConvoHandoff(a *Agent, cfg RunConfig, prompt string) {
	cfg.Prompt = prompt
	a.SetPendingHandoff(cfg, prompt)
	a.SetState(StateRunning)
	m.emit(events.AgentState(a.ID), a)
	if a.isDetached() {
		// A detached agent's stdin is a never-EOF O_RDWR FIFO (see
		// startConvoProcessSurvive) — closing our end does not signal the
		// child, so it must be killed by PID like StopAgent does.
		m.signalKill(a)
	} else {
		a.convo.closeStdinPipe()
	}
	m.logger.Info("agent.convo.handoff", "id", a.ID, "task", cfg.TaskID, "to", cfg.Provider)
}

// reopenConvoHandoffLog reacquires the log writer used to bound a same-agent
// Claude -> per-turn handoff. If the existing detached log cannot be reopened
// for append, fall back to a fresh log file so the persisted provider switch
// still has a marker-bounded segment instead of leaving restart rehydration
// with a switched registry record and no boundary marker.
func (m *Manager) reopenConvoHandoffLog(a *Agent, outFile *os.File) (*os.File, error) {
	if outFile != nil {
		return outFile, nil
	}

	var reopenErr error
	if logPath := a.GetLogPath(); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err == nil {
			return f, nil
		}
		reopenErr = err
	}

	fresh, err := logging.NewAgentOutputFile(m.logDir, a.ID)
	if err != nil {
		if reopenErr != nil {
			return nil, fmt.Errorf("reopen handoff log: %w; fresh handoff log: %w", reopenErr, err)
		}
		return nil, fmt.Errorf("fresh handoff log: %w", err)
	}
	a.SetLogPath(fresh.Name())
	return fresh, nil
}

// completeConvoHandoff finishes a mid-run persistent-Claude -> per-turn
// provider switch once the killed Claude process has actually exited: it
// writes the provider-marker boundary into the still-open log (closing the
// crash window before the switch lands on disk — see regate's deferPersist),
// persists the switched registry record, then hands the log and a fresh
// prompt channel to the per-turn runner. Returns without finalizing/marking
// the agent done: the new goroutine owns the agent's completion from here.
func (m *Manager) completeConvoHandoff(ctx context.Context, a *Agent, outFile *os.File, cfg RunConfig, prompt string) {
	a.SetExitErr(nil)
	if outFile != nil {
		writeProviderMarkerLine(outFile, a.Provider)
		_ = outFile.Close()
	}
	if m.survives() {
		m.saveRegistry(ctx, a)
	}
	cfg.Prompt = prompt
	a.setPromptChannel(make(chan string, 1))
	m.logger.Info("agent.convo.handoff.start", "id", a.ID, "provider", a.Provider)
	go m.runPerTurnConversational(ctx, a, cfg, false)
}

func (m *Manager) runConvoAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64) (retry bool, err error) {
	if outFile == nil {
		return false, nil
	}
	// Detached survival for interactive Claude agents (this path is only
	// reached for Claude; codex/copilot use runPerTurnConversational). A one-shot run
	// passes its prompt as an argument (no stdin); interactive sessions use a
	// never-EOF FIFO for follow-ups.
	if m.survives() {
		if cfg.OneShot {
			return m.runConvoAttemptSurviveOneShot(ctx, a, cfg, outFile, tailOffset)
		}
		return m.runConvoAttemptSurvive(ctx, a, cfg, outFile, tailOffset)
	}
	cmd, stdout, stderrBuf, startErr := m.startConvoProcess(ctx, a, cfg)
	if startErr != nil {
		return false, startErr
	}
	a.SetCmd(cmd)
	m.logger.Info("agent.convo.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	// Send initial prompt when no session exists yet. On retries with a
	// session ID, --resume re-establishes the session so re-sending is wrong.
	if cfg.Prompt != "" && a.GetSessionID() == "" {
		if err := m.writeUserMessage(a, cfg.Prompt); err != nil {
			m.logger.Error("agent.convo.initial-prompt", "id", a.ID, "err", err)
		}
	} else if cfg.Prompt == "" && a.GetSessionID() == "" {
		// Chat sessions can start without an initial prompt — claude is
		// then idle on stdin waiting for the first user message. Reflect
		// that by flipping to Paused so the chat input is enabled.
		a.SetState(StatePaused)
		m.emit(events.AgentState(a.ID), a)
	}

	// Open log file on first successful start; subsequent retries append.
	freshLog := *outFile == nil
	if freshLog {
		f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
		if fileErr != nil {
			m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
		}
		if f != nil {
			a.SetLogPath(f.Name())
			*outFile = f
		}
	}

	var logWriter io.Writer
	if *outFile != nil {
		logWriter = *outFile
	}
	if freshLog {
		// Records the starting provider up front so a mid-run
		// regateBeforeClaudeTurn switch away from Claude can be told apart
		// from this initial segment on rehydration (see
		// rehydratePerTurnConvoFromLog).
		writeProviderMarkerLine(logWriter, a.Provider)
	}

	prevLen := len(a.ConvoOutput())
	m.streamConvoOutput(ctx, a, stdout, logWriter, cfg.OneShot)

	waitErr := cmd.Wait()
	stderrOut := stderrBuf.String()
	attemptEvents := attemptEventsFrom(a.ConvoOutput(), prevLen)
	if waitErr != nil {
		a.SetExitErr(waitErr)
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)

		if shouldRetryConvo(stderrOut, attemptEvents, m.logger) {
			logAttemptStderr(m.logger, "agent.convo.stderr", a.ID, stderrOut, a.GetExitErr())
			return true, nil
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	} else {
		a.SetExitErr(nil)
		if m.reportCleanProviderHealthSignalConvo(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit {
			a.SetExitErr(errProviderRateLimited)
		}
	}
	logAttemptStderr(m.logger, "agent.convo.stderr", a.ID, stderrOut, a.GetExitErr())
	return false, nil
}

// convoEmitState carries the throttle state across processConvoLine calls
// so the live streamer and the file tailer share one emit cadence.
type convoEmitState struct {
	lastEmit time.Time
	pending  *ConvoEvent // latest event buffered for the next emit window
}

// flush emits any buffered event. Called when the stream ends.
func (m *Manager) flushConvo(a *Agent, st *convoEmitState) {
	if st.pending != nil {
		m.emit(events.AgentConvo(a.ID), *st.pending)
		st.pending = nil
	}
}

func (m *Manager) streamConvoOutput(ctx context.Context, a *Agent, stdout io.Reader, outFile io.Writer, oneShot bool) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	st := &convoEmitState{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}
		m.processConvoLine(ctx, a, line, st, oneShot)
	}
	m.flushConvo(a, st)
}

// processConvoLine parses one conversational NDJSON line, appends and
// throttle-emits the event, and runs the session/result/queue/one-shot
// state machine. Shared by the pipe-backed streamer and the survival file
// tailer; it never writes the log file (caller or child owns that).
func (m *Manager) processConvoLine(ctx context.Context, a *Agent, line []byte, st *convoEmitState, oneShot bool) {
	if _, ok := parseProviderMarkerLine(line); ok {
		return
	}
	parsed, parseErr := ParseClaudeLine(line)
	if parseErr != nil {
		m.logger.Warn("agent.convo.parse", "id", a.ID, "err", parseErr, "line", string(line))
		return
	}
	event := claudeEventToConvoEvent(parsed)
	if event.Type == "" {
		return
	}

	a.AppendConvo(event)

	// Always emit result/system events immediately. For others, buffer the
	// latest and emit at most once per convoEmitInterval so the frontend
	// still gets every meaningful update.
	switch {
	case event.Type == "result" || event.Type == "system":
		st.pending = nil
		m.emit(events.AgentConvo(a.ID), event)
		st.lastEmit = time.Now()
	case time.Since(st.lastEmit) >= convoEmitInterval:
		if st.pending != nil {
			m.emit(events.AgentConvo(a.ID), *st.pending)
			st.pending = nil
		}
		m.emit(events.AgentConvo(a.ID), event)
		st.lastEmit = time.Now()
	default:
		e := event
		st.pending = &e
	}

	switch event.Type {
	case "system":
		if event.SessionID != "" && a.GetSessionID() != event.SessionID {
			// Capture the session id as soon as it appears so a restart can
			// resume the conversation. Only persist a registry record for a
			// detached (FIFO-backed survival) agent — a one-shot or legacy
			// interactive agent must not leave a record, or reattach would
			// mis-recover it as a survivable session and stall the workflow.
			a.SetSessionID(event.SessionID)
			if a.isDetached() {
				m.saveRegistry(ctx, a)
			}
		}
	case "result":
		costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
		a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
		m.logger.Info("agent.convo.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow)
		// Drain any prompts queued mid-turn before flipping to paused. Each
		// queued prompt fires the next turn back-to-back so the user's
		// chat-window queue executes in order without manual re-trigger. This
		// turn boundary must re-gate exactly like SendMessage's idle path —
		// otherwise a provider that capped while the turn was running would
		// still receive the queued follow-up on its stranded stdin.
		if next, ok := a.PopPendingPrompt(); ok {
			if err := m.advanceClaudeTurn(ctx, a, next); err != nil {
				m.logger.Error("agent.convo.flush-queue", "id", a.ID, "err", err)
			} else {
				m.logger.Info("agent.convo.queue-flushed", "id", a.ID, "remaining", a.PendingPromptCount())
			}
		} else {
			a.SetState(StatePaused)
			m.emit(events.AgentState(a.ID), a)
		}
		// One-shot runs close stdin so the claude process sees EOF and exits.
		if oneShot {
			m.logger.Info("agent.convo.one-shot-close", "id", a.ID)
			a.convo.closeStdinPipe()
		}
	}
}

// encodeUserMessage renders a user message as a newline-terminated
// stream-json line for claude's stdin.
func encodeUserMessage(text string) ([]byte, error) {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	return append(data, '\n'), nil
}

// writeUserMessage writes a user message to the agent's stdin in stream-json format.
func (m *Manager) writeUserMessage(a *Agent, text string) error {
	data, err := encodeUserMessage(text)
	if err != nil {
		return err
	}

	return a.convo.writeStdin(data)
}

// claudeRegateConfig builds the RunConfig used to re-gate a persistent
// Claude agent at a turn boundary (SendMessage's idle path and
// advanceClaudeTurn's queued-flush path share this so both judge/switch
// provider health from identical agent state).
func claudeRegateConfig(a *Agent) RunConfig {
	return RunConfig{
		TaskID:             a.TaskID,
		Dir:                a.sessionCWD,
		Provider:           a.Provider,
		Model:              a.Model,
		RequirePermissions: a.requirePermissions,
	}
}

// advanceClaudeTurn is the single persistent-Claude turn-boundary
// chokepoint: given the next prompt to send — either SendMessage's idle
// turn boundary or a queued follow-up flushed after a "result" event — it
// re-gates provider health before writing to Claude's stdin, so a provider
// that capped since the last turn does not strand this session. On any
// outcome that cannot proceed (no healthy peer, write failure, cancellation,
// stopped agent), prompt is restored to the front of the pending queue so a
// later attempt retries it in order instead of losing or reordering it.
func (m *Manager) advanceClaudeTurn(ctx context.Context, a *Agent, prompt string) error {
	if a.WasStopped() || ctx.Err() != nil {
		a.RestorePendingPrompt(prompt)
		return fmt.Errorf("agent %s: not advancing turn, stopped or canceled", a.ID)
	}

	cfg := claudeRegateConfig(a)
	updatedCfg, switched, regateErr := m.regateBeforeClaudeTurn(ctx, a, cfg)
	if regateErr != nil {
		// No healthy peer: regate already set a.SetError("rate_limit", ...).
		// Restore the prompt so it is retried (not lost) once a peer or
		// Claude itself recovers, and park the session instead of stranding
		// it mid-write.
		a.RestorePendingPrompt(prompt)
		a.SetState(StatePaused)
		m.emit(events.AgentState(a.ID), a)
		m.logger.Warn("agent.convo.regate.blocked", "id", a.ID, "task", a.TaskID, "err", regateErr)
		return regateErr
	}
	if switched {
		m.beginConvoHandoff(a, updatedCfg, prompt)
		return nil
	}
	if err := m.writeUserMessage(a, prompt); err != nil {
		a.RestorePendingPrompt(prompt)
		a.SetState(StatePaused)
		m.emit(events.AgentState(a.ID), a)
		m.logger.Error("agent.convo.write", "id", a.ID, "err", err)
		return err
	}
	a.SetState(StateRunning)
	m.emit(events.AgentState(a.ID), a)
	return nil
}

// SendMessage sends a follow-up user message to a conversational agent.
// When the agent is mid-turn (StateRunning), the message is appended to a
// pending queue and flushed on the next "result" event, so users can pile
// up follow-ups without waiting for each turn to settle.
func (m *Manager) SendMessage(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.Mode != "interactive" {
		return fmt.Errorf("agent %s is not in interactive/conversational mode", agentID)
	}
	if !a.convo.hasStdinPipe() {
		return fmt.Errorf("agent %s has no stdin pipe (not conversational)", agentID)
	}

	queued := a.GetState() == StateRunning
	if queued {
		a.EnqueuePrompt(text)
		m.logger.Info("agent.convo.message_queued", "id", a.ID, "queue_len", a.PendingPromptCount())
	} else {
		// Turn boundary: re-consult provider health before writing to the
		// idle Claude session's stdin, so a provider that capped since the
		// last turn does not strand this chat for the rest of its life.
		if err := m.advanceClaudeTurn(m.ctx, a, text); err != nil {
			return fmt.Errorf("agent %s: %w", agentID, err)
		}
		m.logger.Info("agent.convo.message_sent", "id", a.ID)
	}

	// Add user message to convo buffer regardless — the user should see
	// their message immediately, even if it is still queued.
	ev := ConvoEvent{
		Type:      "user_input",
		Text:      text,
		Timestamp: time.Now().UTC(),
	}
	a.AppendConvo(ev)
	m.emit(events.AgentConvo(a.ID), ev)
	return nil
}

// GetConvoOutput returns the full conversation event buffer for an agent.
func (m *Manager) GetConvoOutput(agentID string) ([]ConvoEvent, error) {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return a.ConvoOutput(), nil
}
