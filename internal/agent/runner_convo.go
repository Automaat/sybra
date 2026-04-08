package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	if a.SessionID != "" {
		args = append(args, "--resume", a.SessionID)
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

func (m *Manager) startConvoProcess(ctx context.Context, a *Agent, cfg RunConfig) (*exec.Cmd, io.ReadCloser, error) {
	args := m.buildConvoArgs(a, cfg)
	cmd := exec.CommandContext(ctx, "claude", args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	a.stdinPipe = stdinPipe

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start claude: %w", err)
	}
	return cmd, stdout, nil
}

func (m *Manager) runConversational(ctx context.Context, a *Agent, cfg RunConfig) {
	cmd, stdout, err := m.startConvoProcess(ctx, a, cfg)
	if err != nil {
		m.handleError(a, err)
		return
	}
	a.cmd = cmd
	a.PID = cmd.Process.Pid
	m.logger.Info("agent.convo.start", "id", a.ID, "pid", a.PID, "dir", cmd.Dir)

	// Send initial prompt.
	if cfg.Prompt != "" {
		if err := m.writeUserMessage(a, cfg.Prompt); err != nil {
			m.logger.Error("agent.convo.initial-prompt", "id", a.ID, "err", err)
		}
	}

	outFile, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
	if fileErr != nil {
		m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
	}
	if outFile != nil {
		a.LogPath = outFile.Name()
		defer func() { _ = outFile.Close() }()
	}

	var logWriter io.Writer
	if outFile != nil {
		logWriter = outFile
	}
	m.streamConvoOutput(a, stdout, logWriter)

	waitErr := cmd.Wait()
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)
		a.ExitErr = waitErr
	}

	a.State = StateStopped
	if a.done != nil {
		close(a.done)
	}
	m.logger.Info("agent.convo.done", "id", a.ID, "cost", a.CostUSD)
	m.emit(events.AgentState(a.ID), a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

func (m *Manager) streamConvoOutput(a *Agent, stdout io.Reader, outFile io.Writer) {
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

		a.convoBuffer = append(a.convoBuffer, event)
		a.LastEventAt = time.Now().UTC()

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
				a.SessionID = event.SessionID
			}
		case "result":
			if event.SessionID != "" {
				a.SessionID = event.SessionID
			}
			a.CostUSD += event.CostUSD
			a.InputTokens += event.InputTokens
			a.OutputTokens += event.OutputTokens
			m.logger.Info("agent.convo.result", "id", a.ID, "session_id", event.SessionID, "cost", a.CostUSD)
			// After result, agent is idle waiting for next user message.
			a.State = StatePaused
			m.emit(events.AgentState(a.ID), a)
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

	eventType, _ := raw["type"].(string)
	event := ConvoEvent{
		Type:      eventType,
		Subtype:   strVal(raw, "subtype"),
		Timestamp: time.Now().UTC(),
		Raw:       json.RawMessage(line),
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
	if a.stdinPipe == nil {
		return fmt.Errorf("agent %s has no stdin pipe (not conversational)", agentID)
	}
	if err := m.writeUserMessage(a, text); err != nil {
		return err
	}
	a.State = StateRunning
	m.emit(events.AgentState(a.ID), a)
	m.logger.Info("agent.convo.message_sent", "id", a.ID)

	// Add user message to convo buffer.
	a.convoBuffer = append(a.convoBuffer, ConvoEvent{
		Type:      "user_input",
		Text:      text,
		Timestamp: time.Now().UTC(),
	})
	m.emit(events.AgentConvo(a.ID), ConvoEvent{
		Type:      "user_input",
		Text:      text,
		Timestamp: time.Now().UTC(),
	})
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
