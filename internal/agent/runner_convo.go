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

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/logging"
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
		}
	}
	return ev
}

// convoEmitInterval caps event emission rate for conversational agents.
const convoEmitInterval = 50 * time.Millisecond

func (m *Manager) buildConvoArgs(a *Agent, cfg RunConfig) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if cfg.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.PermissionMode)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else if !cfg.RequirePermissions && cfg.PermissionMode == "" {
		args = append(args, "--dangerously-skip-permissions")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	// Only wire the approval hook for agents that actually need permission checks.
	// Agents with --dangerously-skip-permissions should not be blocked by the hook.
	needsApproval := cfg.RequirePermissions || cfg.PermissionMode != ""
	if m.approvalAddr != "" && needsApproval {
		hookSettings := fmt.Sprintf(
			`{"hooks":{"PreToolUse":[{"matcher":"","hooks":[{"type":"http","url":"http://%s/hooks/pre-tool-use","timeout":300}]}]}}`,
			m.approvalAddr,
		)
		args = append(args, "--settings", hookSettings)
	}
	return args
}

func (m *Manager) startConvoProcess(ctx context.Context, a *Agent, cfg RunConfig) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	args := m.buildConvoArgs(a, cfg)
	cmd := exec.CommandContext(ctx, "claude", args...)
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
	a.stdinMu.Lock()
	a.stdinPipe = stdinPipe
	a.stdinMu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderrBuf := new(bytes.Buffer)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
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

	for attempt := range len(headlessRetryBackoffs) + 1 {
		if attempt > 0 {
			wait := headlessRetryBackoffs[attempt-1]
			m.logger.Info("agent.convo.retry", "id", a.ID, "attempt", attempt, "backoff", wait)
			select {
			case <-ctx.Done():
				goto done
			case <-time.After(wait):
			}
		}

		retry, fatalErr := m.runConvoAttempt(ctx, a, cfg, &outFile)
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
	m.markAgentDone(a)
	m.logger.Info("agent.convo.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, a.GetExitErr() == nil)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

func (m *Manager) runConvoAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File) (retry bool, err error) {
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
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)

		all := a.ConvoOutput()
		if prevLen > len(all) {
			prevLen = len(all)
		}
		attemptEvents := all[prevLen:]
		if shouldRetryConvo(stderrOut, attemptEvents, m.logger) {
			return true, nil
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	}
	return false, nil
}

func (m *Manager) streamConvoOutput(a *Agent, stdout io.Reader, outFile io.Writer, oneShot bool) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var lastEmit time.Time
	var pending *ConvoEvent // buffered event waiting for next emit window

	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		parsed, parseErr := ParseClaudeLine(line)
		if parseErr != nil {
			m.logger.Warn("agent.convo.parse", "id", a.ID, "err", parseErr, "line", string(line))
			continue
		}
		event := claudeEventToConvoEvent(parsed)
		if event.Type == "" {
			continue
		}

		a.AppendConvo(event)

		// Always emit result/system events immediately. For others, buffer
		// the latest and emit at most once per convoEmitInterval so the
		// frontend still gets every meaningful update.
		switch {
		case event.Type == "result" || event.Type == "system":
			pending = nil
			m.emit(events.AgentConvo(a.ID), event)
			lastEmit = time.Now()
		case time.Since(lastEmit) >= convoEmitInterval:
			if pending != nil {
				m.emit(events.AgentConvo(a.ID), *pending)
				pending = nil
			}
			m.emit(events.AgentConvo(a.ID), event)
			lastEmit = time.Now()
		default:
			// Buffer the latest event; it will be emitted on the next window.
			e := event
			pending = &e
		}

		switch event.Type {
		case "system":
			if event.SessionID != "" {
				a.SetSessionID(event.SessionID)
			}
		case "result":
			costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens)
			m.logger.Info("agent.convo.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow)
			// Drain any prompts queued mid-turn before flipping to paused.
			// Each queued prompt fires the next turn back-to-back so the
			// user's chat-window queue executes in order without manual
			// re-trigger.
			if next, ok := a.PopPendingPrompt(); ok {
				if err := m.writeUserMessage(a, next); err != nil {
					m.logger.Error("agent.convo.flush-queue", "id", a.ID, "err", err)
					a.SetState(StatePaused)
				} else {
					a.SetState(StateRunning)
					m.logger.Info("agent.convo.queue-flushed", "id", a.ID, "remaining", a.PendingPromptCount())
				}
			} else {
				// After result, agent is idle waiting for next user message.
				a.SetState(StatePaused)
			}
			m.emit(events.AgentState(a.ID), a)
			// One-shot runs (workflow steps that expect a single turn) close
			// stdin now so the claude process sees EOF and exits. The scanner
			// loop unwinds on stdout EOF, cmd.Wait() returns, SetState(Stopped)
			// fires, and onComplete advances the workflow to the next step.
			// Without this, interactive agents sit paused forever and never
			// trigger the evaluator.
			if oneShot {
				m.logger.Info("agent.convo.one-shot-close", "id", a.ID)
				a.stdinMu.Lock()
				if a.stdinPipe != nil {
					_ = a.stdinPipe.Close()
					a.stdinPipe = nil
				}
				a.stdinMu.Unlock()
			}
		}
	}

	// Flush any remaining buffered event.
	if pending != nil {
		m.emit(events.AgentConvo(a.ID), *pending)
	}
}

// writeUserMessage writes a user message to the agent's stdin in stream-json format.
func (m *Manager) writeUserMessage(a *Agent, text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')

	a.stdinMu.Lock()
	defer a.stdinMu.Unlock()

	if a.stdinPipe == nil {
		return fmt.Errorf("stdin pipe closed")
	}
	if _, err := a.stdinPipe.Write(data); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
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
	a.stdinMu.Lock()
	hasPipe := a.stdinPipe != nil
	a.stdinMu.Unlock()
	if !hasPipe {
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
