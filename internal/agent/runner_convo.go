package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	cmd := exec.CommandContext(ctx, "claude", args...)
	configureGracefulShutdown(cmd)
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

	for attempt := range len(headlessRetryBackoffs) + 1 {
		if attempt > 0 {
			wait := headlessRetryBackoffs[attempt-1]
			m.logger.Info("agent.convo.retry", "id", a.ID, "attempt", attempt, "backoff", wait)
			select {
			case <-ctx.Done():
				if a.isDetached() && !a.WasStopped() {
					return
				}
				goto done
			case <-time.After(wait):
			}
		}

		retry, fatalErr := m.runConvoAttempt(ctx, a, cfg, &outFile, &tailOffset)
		if errors.Is(fatalErr, errSurviveShutdown) {
			return
		}
		if fatalErr != nil {
			m.handleError(a, fatalErr)
			return
		}
		if !retry {
			break
		}
		if attempt == len(headlessRetryBackoffs) {
			m.logger.Error("agent.convo.retry.exhausted", "id", a.ID, "attempts", len(headlessRetryBackoffs))
		}
	}

done:
	a.SetState(StateStopped)
	m.logger.Info("agent.convo.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(a, a.GetExitErr() == nil)
	m.markAgentDone(a)
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
	if *outFile == nil {
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

	prevLen := len(a.ConvoOutput())
	m.streamConvoOutput(a, stdout, logWriter, cfg.OneShot)

	waitErr := cmd.Wait()
	stderrOut := stderrBuf.String()
	if stderrOut != "" {
		m.logger.Error("agent.convo.stderr", "id", a.ID, "stderr", stderrOut)
	}
	all := a.ConvoOutput()
	if prevLen > len(all) {
		prevLen = len(all)
	}
	attemptEvents := all[prevLen:]
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)

		if shouldRetryConvo(stderrOut, attemptEvents, m.logger) {
			return true, nil
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	} else if m.reportCleanProviderHealthSignalConvo(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
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

func (m *Manager) streamConvoOutput(a *Agent, stdout io.Reader, outFile io.Writer, oneShot bool) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	st := &convoEmitState{}
	for scanner.Scan() {
		line := scanner.Bytes()
		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}
		m.processConvoLine(a, line, st, oneShot)
	}
	m.flushConvo(a, st)
}

// processConvoLine parses one conversational NDJSON line, appends and
// throttle-emits the event, and runs the session/result/queue/one-shot
// state machine. Shared by the pipe-backed streamer and the survival file
// tailer; it never writes the log file (caller or child owns that).
func (m *Manager) processConvoLine(a *Agent, line []byte, st *convoEmitState, oneShot bool) {
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
				m.saveRegistry(a)
			}
		}
	case "result":
		costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
		a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
		m.logger.Info("agent.convo.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow)
		// Drain any prompts queued mid-turn before flipping to paused. Each
		// queued prompt fires the next turn back-to-back so the user's
		// chat-window queue executes in order without manual re-trigger.
		if next, ok := a.PopPendingPrompt(); ok {
			if err := m.writeUserMessage(a, next); err != nil {
				m.logger.Error("agent.convo.flush-queue", "id", a.ID, "err", err)
				a.SetState(StatePaused)
			} else {
				a.SetState(StateRunning)
				m.logger.Info("agent.convo.queue-flushed", "id", a.ID, "remaining", a.PendingPromptCount())
			}
		} else {
			a.SetState(StatePaused)
		}
		m.emit(events.AgentState(a.ID), a)
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
		if err := m.writeUserMessage(a, text); err != nil {
			return err
		}
		a.SetState(StateRunning)
		m.emit(events.AgentState(a.ID), a)
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
