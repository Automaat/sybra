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

// headlessEmitInterval caps per-agent stream event emission rate.
// Result events always emit immediately (terminal signal). Frontend
// subscribers may miss intermediate events but can recover via
// GetAgentOutput which reads from outputBuffer.
const headlessEmitInterval = 50 * time.Millisecond

var headlessRetryBackoffs = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

func (m *Manager) runHeadless(ctx context.Context, a *Agent, prompt string, allowedTools []string, requirePermissions bool) {
	// outFile is opened lazily on first successful cmd.Start and shared across
	// retry attempts so all output lands in one file. Closed on function exit.
	var outFile *os.File
	defer func() {
		if outFile != nil {
			_ = outFile.Close()
		}
	}()

	for attempt := range len(headlessRetryBackoffs) + 1 {
		if attempt > 0 {
			wait := headlessRetryBackoffs[attempt-1]
			m.logger.Info("agent.headless.retry", "id", a.ID, "attempt", attempt, "backoff", wait)
			select {
			case <-ctx.Done():
				goto done
			case <-time.After(wait):
			}
		}

		retry, fatalErr := m.runHeadlessAttempt(ctx, a, prompt, allowedTools, requirePermissions, &outFile)
		if fatalErr != nil {
			m.handleError(a, fatalErr)
			return
		}
		if !retry {
			break
		}
		if attempt == len(headlessRetryBackoffs) {
			m.logger.Error("agent.headless.retry.exhausted", "id", a.ID, "attempts", len(headlessRetryBackoffs))
		}
	}

done:
	a.State = StateStopped
	if a.done != nil {
		close(a.done)
	}
	m.logger.Info("agent.headless.done", "id", a.ID, "cost", a.CostUSD)
	m.emit(events.AgentState(a.ID), a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

func (m *Manager) runHeadlessAttempt(ctx context.Context, a *Agent, prompt string, allowedTools []string, requirePermissions bool, outFile **os.File) (retry bool, err error) {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}

	if a.SessionID != "" {
		args = append(args, "--resume", a.SessionID)
	}

	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	} else if !requirePermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	a.cmd = cmd

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return false, fmt.Errorf("stdout pipe: %w", pipeErr)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		return false, fmt.Errorf("start claude: %w", startErr)
	}

	// Open log file on first successful start; subsequent retries append to same file.
	if *outFile == nil {
		f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
		if fileErr != nil {
			m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
		}
		if f != nil {
			a.LogPath = f.Name()
			*outFile = f
		}
	}

	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	var logWriter io.Writer
	if *outFile != nil {
		logWriter = *outFile
	}

	prevLen := len(a.outputBuffer)
	m.streamHeadlessOutput(ctx, a, stdout, logWriter)

	waitErr := cmd.Wait()

	stderrOut := stderrBuf.String()
	if stderrOut != "" {
		m.logger.Error("agent.headless.stderr", "id", a.ID, "stderr", stderrOut)
	}
	if waitErr != nil {
		m.logger.Error("agent.headless.exit", "id", a.ID, "err", waitErr)
		a.ExitErr = waitErr
	}

	if waitErr != nil && is529Error(stderrOut, a.outputBuffer[prevLen:]) {
		return true, nil
	}
	return false, nil
}

// is529Error detects Anthropic API 529 (overloaded) from stderr output or stream events.
func is529Error(stderrOut string, streamEvents []StreamEvent) bool {
	lower := strings.ToLower(stderrOut)
	if strings.Contains(lower, "529") || strings.Contains(lower, "overloaded") {
		return true
	}
	for _, e := range streamEvents {
		if e.Type == "result" && e.Subtype == "error" {
			c := strings.ToLower(e.Content)
			if strings.Contains(c, "529") || strings.Contains(c, "overloaded") {
				return true
			}
		}
	}
	return false
}

func (m *Manager) streamHeadlessOutput(ctx context.Context, a *Agent, stdout io.Reader, outFile io.Writer) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var lastEmit time.Time
	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		event, err := parseStreamEvent(line)
		if err != nil {
			m.logger.Warn("agent.headless.parse", "id", a.ID, "err", err, "line", string(line))
			continue
		}
		if event.Type == "" {
			continue
		}

		a.outputBuffer = append(a.outputBuffer, event)
		a.LastEventAt = time.Now().UTC()
		if event.Type == "result" || time.Since(lastEmit) >= headlessEmitInterval {
			m.emit(events.AgentOutput(a.ID), event)
			lastEmit = time.Now()
		}

		if event.Type == "assistant" {
			a.TurnCount++
			m.mu.RLock()
			maxTurns := m.guardrails.MaxTurns
			m.mu.RUnlock()
			if maxTurns > 0 && a.TurnCount >= maxTurns {
				m.logger.Warn("agent.guardrail.turns", "id", a.ID, "turns", a.TurnCount, "limit", maxTurns)
				a.EscalationReason = "turns"
				m.emit(events.AgentEscalation(a.ID), EscalationEvent{
					Reason:    "turns",
					TurnCount: a.TurnCount,
					Limit:     float64(maxTurns),
				})
				m.emit(events.AgentState(a.ID), a)
				// Block until human responds or context is cancelled.
				select {
				case continueRun := <-a.escalationCh:
					if !continueRun {
						a.cancel()
						return
					}
					// Human approved continuation — clear reason and keep going.
					a.EscalationReason = ""
					m.emit(events.AgentState(a.ID), a)
				case <-ctx.Done():
					return
				}
			}
		}

		if event.Type == "result" {
			a.SessionID = event.SessionID
			a.CostUSD += event.CostUSD
			a.InputTokens += event.InputTokens
			a.OutputTokens += event.OutputTokens
			m.logger.Info("agent.headless.result", "id", a.ID, "session_id", event.SessionID, "cost", a.CostUSD)
			m.mu.RLock()
			maxCost := m.guardrails.MaxCostUSD
			m.mu.RUnlock()
			if maxCost > 0 && a.CostUSD > maxCost {
				m.logger.Warn("agent.guardrail.cost", "id", a.ID, "cost", a.CostUSD, "limit", maxCost)
				a.EscalationReason = "cost"
				m.emit(events.AgentEscalation(a.ID), EscalationEvent{
					Reason:  "cost",
					CostUSD: a.CostUSD,
					Limit:   maxCost,
				})
				m.emit(events.AgentState(a.ID), a)
			}
		}
	}
}

func parseStreamEvent(line []byte) (StreamEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return StreamEvent{}, fmt.Errorf("unmarshal stream event: %w", err)
	}

	// ok intentionally discarded: missing/non-string type yields "" which is handled by the default case.
	eventType, _ := raw["type"].(string)
	event := StreamEvent{
		Type:    eventType,
		Subtype: strVal(raw, "subtype"),
	}

	switch eventType {
	case "system":
		// ok intentionally discarded: zero-value "" is safe when session_id is absent.
		event.SessionID, _ = raw["session_id"].(string)

	case "assistant":
		event.Content = extractMessageContent(raw)

	case "user":
		event.Content = extractToolResult(raw)

	case "result":
		// ok intentionally discarded on string assertions: zero-value "" is acceptable when field is absent.
		event.Content, _ = raw["result"].(string)
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
		// rate_limit_event, etc — keep type, no content
	}

	return event, nil
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
	if a.done != nil {
		close(a.done)
	}
	m.logger.Error("agent.error", "id", a.ID, "err", err)
	m.emit(events.AgentError(a.ID), err.Error())
	if m.onComplete != nil {
		m.onComplete(a)
	}
}
