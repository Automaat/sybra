package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Automaat/synapse/internal/logging"
)

func (m *Manager) runHeadless(ctx context.Context, a *Agent, prompt string, allowedTools []string) {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}

	if a.SessionID != "" {
		args = append(args, "--resume", a.SessionID)
	}

	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	a.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.handleError(a, fmt.Errorf("stdout pipe: %w", err))
		return
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		m.handleError(a, fmt.Errorf("start claude: %w", err))
		return
	}

	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	outFile, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
	if fileErr != nil {
		m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
	}
	if outFile != nil {
		defer func() { _ = outFile.Close() }()
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.WriteString("\n")
		}

		event := parseStreamEvent(line)
		if event.Type == "" {
			continue
		}

		a.outputBuffer = append(a.outputBuffer, event)
		m.emit("agent:output:"+a.ID, event)

		if event.Type == "result" {
			a.SessionID = event.SessionID
			a.CostUSD += event.CostUSD
			m.logger.Info("agent.headless.result", "id", a.ID, "session_id", event.SessionID, "cost", a.CostUSD)
		}
	}

	waitErr := cmd.Wait()

	if stderrOut := stderrBuf.String(); stderrOut != "" {
		m.logger.Error("agent.headless.stderr", "id", a.ID, "stderr", stderrOut)
	}
	if waitErr != nil {
		m.logger.Error("agent.headless.exit", "id", a.ID, "err", waitErr)
	}

	a.State = StateStopped
	m.logger.Info("agent.headless.done", "id", a.ID, "cost", a.CostUSD)
	m.emit("agent:state:"+a.ID, a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

func parseStreamEvent(line []byte) StreamEvent {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return StreamEvent{}
	}

	eventType, _ := raw["type"].(string)
	event := StreamEvent{
		Type:    eventType,
		Subtype: strVal(raw, "subtype"),
	}

	switch eventType {
	case "system":
		event.SessionID, _ = raw["session_id"].(string)

	case "assistant":
		event.Content = extractMessageContent(raw)

	case "user":
		event.Content = extractToolResult(raw)

	case "result":
		event.Content, _ = raw["result"].(string)
		event.SessionID, _ = raw["session_id"].(string)
		if cost, ok := raw["total_cost_usd"].(float64); ok {
			event.CostUSD = cost
		}

	default:
		// rate_limit_event, etc — keep type, no content
	}

	return event
}

func extractMessageContent(raw map[string]any) string {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		case "tool_use":
			name, _ := block["name"].(string)
			input, _ := block["input"].(map[string]any)
			desc, _ := input["description"].(string)
			cmd, _ := input["command"].(string)
			switch {
			case desc != "":
				parts = append(parts, fmt.Sprintf("[%s] %s", name, desc))
			case cmd != "":
				parts = append(parts, fmt.Sprintf("[%s] %s", name, cmd))
			default:
				parts = append(parts, fmt.Sprintf("[%s]", name))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractToolResult(raw map[string]any) string {
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, c := range content {
		block, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["content"].(string); ok && text != "" {
			// Truncate long tool results
			if len(text) > 500 {
				text = text[:500] + "..."
			}
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func strVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func (m *Manager) handleError(a *Agent, err error) {
	a.State = StateStopped
	m.logger.Error("agent.error", "id", a.ID, "err", err)
	m.emit("agent:error:"+a.ID, err.Error())
}
