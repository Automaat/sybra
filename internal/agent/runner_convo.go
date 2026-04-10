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
	if a.done != nil {
		close(a.done)
	}
	m.logger.Info("agent.convo.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
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
		if shouldRetryConvo(stderrOut, all[prevLen:], m.logger) {
			return true, nil
		}
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

		event, err := parseConvoEvent(line)
		if err != nil {
			m.logger.Warn("agent.convo.parse", "id", a.ID, "err", err, "line", string(line))
			continue
		}
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
			// After result, agent is idle waiting for next user message.
			a.SetState(StatePaused)
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

func parseConvoEvent(line []byte) (ConvoEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return ConvoEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	// scanner.Bytes() aliases the scanner's internal buffer which is
	// overwritten on the next Scan() call. Copy the bytes into the
	// ConvoEvent so Raw stays valid after the scanner advances and
	// downstream marshaling (frontend events, log writes) sees the
	// original line rather than a stale/partial one. Without the copy
	// callers hit: json: error calling MarshalJSON for type
	// json.RawMessage: invalid character '{' after top-level value.
	rawCopy := make([]byte, len(line))
	copy(rawCopy, line)

	eventType, _ := raw["type"].(string)
	event := ConvoEvent{
		Type:      eventType,
		Subtype:   strVal(raw, "subtype"),
		Timestamp: time.Now().UTC(),
		Raw:       rawCopy,
	}

	switch eventType {
	case "system":
		event.SessionID, _ = raw["session_id"].(string)

	case "assistant":
		msg, _ := raw["message"].(map[string]any)
		if msg != nil {
			event.Text, event.ToolUses = extractConvoAssistant(msg)
		}
		event.SessionID = strVal(raw, "session_id")

	case "user":
		msg, _ := raw["message"].(map[string]any)
		if msg != nil {
			event.ToolResults = extractConvoToolResults(msg)
		}

	case "result":
		event.Text, _ = raw["result"].(string)
		event.SessionID, _ = raw["session_id"].(string)
		if cost, ok := raw["total_cost_usd"].(float64); ok {
			event.CostUSD = cost
		}
		if v, ok := raw["total_input_tokens"].(float64); ok {
			event.InputTokens = int(v)
		}
		if v, ok := raw["total_output_tokens"].(float64); ok {
			event.OutputTokens = int(v)
		}
		// Parse structured error envelope: {"type":"overloaded_error","status":529,...}
		if errBlock, ok := raw["error"].(map[string]any); ok {
			event.ErrorType, _ = errBlock["type"].(string)
			if status, ok := errBlock["status"].(float64); ok {
				event.ErrorStatus = int(status)
			}
		}

	default:
		// rate_limit_event etc — preserve type only
	}

	return event, nil
}

func extractConvoAssistant(msg map[string]any) (string, []ToolUseBlock) {
	content, ok := msg["content"].([]any)
	if !ok {
		return "", nil
	}
	var textParts []string
	var tools []ToolUseBlock

	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok {
				textParts = append(textParts, text)
			}
		case "tool_use":
			tb := ToolUseBlock{
				ID:   strVal(block, "id"),
				Name: strVal(block, "name"),
			}
			if input, ok := block["input"].(map[string]any); ok {
				tb.Input = input
			}
			tools = append(tools, tb)
		}
	}
	return strings.Join(textParts, "\n"), tools
}

func extractConvoToolResults(msg map[string]any) []ToolResultBlock {
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var results []ToolResultBlock
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] != "tool_result" {
			continue
		}
		tr := ToolResultBlock{
			ToolUseID: strVal(block, "tool_use_id"),
		}
		if isErr, ok := block["is_error"].(bool); ok {
			tr.IsError = isErr
		}
		// Content can be string or array of blocks.
		switch v := block["content"].(type) {
		case string:
			tr.Content = v
		case []any:
			var parts []string
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			tr.Content = strings.Join(parts, "\n")
		}
		if len(tr.Content) > 2000 {
			tr.Content = tr.Content[:2000] + "..."
		}
		results = append(results, tr)
	}
	return results
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
	if err := m.writeUserMessage(a, text); err != nil {
		return err
	}
	a.SetState(StateRunning)
	m.emit(events.AgentState(a.ID), a)
	m.logger.Info("agent.convo.message_sent", "id", a.ID)

	// Add user message to convo buffer.
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
