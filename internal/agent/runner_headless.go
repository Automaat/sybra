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

func (m *Manager) runHeadless(ctx context.Context, a *Agent, cfg RunConfig) {
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

		retry, fatalErr := m.runHeadlessAttempt(ctx, a, cfg, &outFile)
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
	a.SetState(StateStopped)
	if a.done != nil {
		close(a.done)
	}
	m.logger.Info("agent.headless.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	if m.onComplete != nil {
		m.onComplete(a)
	}
}

func (m *Manager) runHeadlessAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File) (retry bool, err error) {
	name, args, command, err := buildHeadlessInvocation(a, cfg)
	if err != nil {
		return false, err
	}

	cmd := exec.CommandContext(ctx, name, args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	a.Command = command

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return false, fmt.Errorf("stdout pipe: %w", pipeErr)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		return false, fmt.Errorf("start %s: %w", name, startErr)
	}
	a.SetCmd(cmd)

	// Open log file on first successful start; subsequent retries append to same file.
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

	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	var logWriter io.Writer
	if *outFile != nil {
		logWriter = *outFile
	}

	prevLen := len(a.Output())
	m.streamHeadlessOutput(ctx, a, stdout, logWriter)

	waitErr := cmd.Wait()

	stderrOut := stderrBuf.String()
	if stderrOut != "" {
		m.logger.Error("agent.headless.stderr", "id", a.ID, "stderr", stderrOut)
	}
	if waitErr != nil {
		m.logger.Error("agent.headless.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
	}

	if waitErr != nil {
		// Only inspect the events produced during this attempt.
		all := a.Output()
		if prevLen > len(all) {
			prevLen = len(all)
		}
		if a.Provider == "claude" && is529Error(stderrOut, all[prevLen:]) {
			return true, nil
		}
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

		event, err := parseStreamEvent(a.Provider, line)
		if err != nil {
			m.logger.Warn("agent.headless.parse", "id", a.ID, "err", err, "line", string(line))
			continue
		}
		if event.Type == "" {
			continue
		}

		a.AppendOutput(event)
		if event.Type == "result" || time.Since(lastEmit) >= headlessEmitInterval {
			m.emit(events.AgentOutput(a.ID), event)
			lastEmit = time.Now()
		}

		if event.Type == "assistant" {
			turns := a.IncTurnCount()
			m.mu.RLock()
			maxTurns := m.guardrails.MaxTurns
			m.mu.RUnlock()
			if maxTurns > 0 && turns >= maxTurns {
				m.logger.Warn("agent.guardrail.turns", "id", a.ID, "turns", turns, "limit", maxTurns)
				a.SetEscalationReason("turns")
				m.emit(events.AgentEscalation(a.ID), EscalationEvent{
					Reason:    "turns",
					TurnCount: turns,
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
					a.SetEscalationReason("")
					m.emit(events.AgentState(a.ID), a)
				case <-ctx.Done():
					return
				}
			}
		}

		if event.Type == "result" {
			costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens)
			m.logger.Info("agent.headless.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow)
			m.mu.RLock()
			maxCost := m.guardrails.MaxCostUSD
			m.mu.RUnlock()
			if maxCost > 0 && costNow > maxCost {
				m.logger.Warn("agent.guardrail.cost", "id", a.ID, "cost", costNow, "limit", maxCost)
				a.SetEscalationReason("cost")
				m.emit(events.AgentEscalation(a.ID), EscalationEvent{
					Reason:  "cost",
					CostUSD: costNow,
					Limit:   maxCost,
				})
				m.emit(events.AgentState(a.ID), a)
			}
		}
	}
}

func buildHeadlessInvocation(a *Agent, cfg RunConfig) (name string, args []string, command string, err error) {
	if a.Provider != "claude" && a.Provider != "codex" {
		err = fmt.Errorf("unsupported provider: %s", a.Provider)
		return
	}
	for _, tool := range cfg.AllowedTools {
		if !safeArgRe.MatchString(tool) {
			err = fmt.Errorf("invalid tool %q: must match %s", tool, safeArgRe)
			return
		}
	}
	if a.Model != "" && !safeArgRe.MatchString(a.Model) {
		err = fmt.Errorf("invalid model %q: must match %s", a.Model, safeArgRe)
		return
	}

	if a.Provider == "codex" {
		name = "codex"
		args = []string{"exec", "--json", "--skip-git-repo-check"}
		if !cfg.RequirePermissions {
			args = append(args, "--full-auto")
		} else {
			args = append(args, "--sandbox", "workspace-write")
		}
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		if a.sessionCWD != "" {
			args = append(args, "-C", a.sessionCWD)
		}
		args = append(args, cfg.Prompt)
		command = "codex " + strings.Join(args, " ")
		return
	}

	name = "claude"
	args = []string{"-p", cfg.Prompt, "--output-format", "stream-json", "--verbose"}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--resume", sid)
	}
	if len(cfg.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(cfg.AllowedTools, ","))
	} else if !cfg.RequirePermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	command = "claude " + strings.Join(args, " ")
	return
}

func parseStreamEvent(provider string, line []byte) (StreamEvent, error) {
	if normalizeProvider(provider) == "codex" {
		return parseCodexStreamEvent(line)
	}
	return parseClaudeStreamEvent(line)
}

func parseClaudeStreamEvent(line []byte) (StreamEvent, error) {
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

func parseCodexStreamEvent(line []byte) (StreamEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return StreamEvent{}, fmt.Errorf("unmarshal stream event: %w", err)
	}

	eventType := strVal(raw, "type")
	switch eventType {
	case "thread.started":
		return StreamEvent{Type: "init", SessionID: strVal(raw, "thread_id")}, nil
	case "turn.started":
		return StreamEvent{Type: "init", Content: "Turn started."}, nil
	case "error":
		return StreamEvent{
			Type:    "result",
			Subtype: "error",
			Content: strVal(raw, "message"),
		}, nil
	case "turn.completed":
		usage, _ := raw["usage"].(map[string]any)
		return StreamEvent{
			Type:         "result",
			Content:      "Completed.",
			InputTokens:  int(floatVal(usage, "input_tokens")),
			OutputTokens: int(floatVal(usage, "output_tokens")),
		}, nil
	case "item.started", "item.completed":
		return parseCodexItemEvent(eventType, raw)
	default:
		return StreamEvent{Type: eventType}, nil
	}
}

func parseCodexItemEvent(eventType string, raw map[string]any) (StreamEvent, error) {
	item, _ := raw["item"].(map[string]any)
	itemType := strVal(item, "type")
	switch itemType {
	case "agent_message":
		return StreamEvent{
			Type:    "assistant",
			Content: strVal(item, "text"),
		}, nil
	case "command_execution":
		if eventType == "item.started" {
			return StreamEvent{
				Type:    "tool_use",
				Content: strVal(item, "command"),
			}, nil
		}
		output := strVal(item, "aggregated_output")
		exitCode := int(floatVal(item, "exit_code"))
		if output == "" {
			output = fmt.Sprintf("Command exited with code %d.", exitCode)
		}
		return StreamEvent{
			Type:    "tool_result",
			Content: output,
		}, nil
	default:
		return StreamEvent{
			Type:    "assistant",
			Content: strVal(item, "text"),
		}, nil
	}
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
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func floatVal(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, _ := m[key].(float64)
	return v
}

func (m *Manager) handleError(a *Agent, err error) {
	a.SetState(StateStopped)
	if a.done != nil {
		close(a.done)
	}
	m.logger.Error("agent.error", "id", a.ID, "err", err)
	m.emit(events.AgentError(a.ID), err.Error())
	if m.onComplete != nil {
		m.onComplete(a)
	}
}
