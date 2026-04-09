package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/Automaat/synapse/internal/events"
	"github.com/Automaat/synapse/internal/logging"
)

// runCodexConversational runs Codex in interactive conversational mode.
// Each turn spawns a fresh `codex exec --json` process. After turn.completed
// the agent transitions to StatePaused and waits for the next prompt on
// promptCh. OneShot skips the wait and exits after the first turn.
func (m *Manager) runCodexConversational(ctx context.Context, a *Agent, cfg RunConfig) {
	defer func() {
		a.SetState(StateStopped)
		if a.done != nil {
			close(a.done)
		}
		m.logger.Info("agent.codex.convo.done", "id", a.ID, "cost", a.GetCostUSD())
		m.emit(events.AgentState(a.ID), a)
		if m.onComplete != nil {
			m.onComplete(a)
		}
	}()

	outFile, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
	if fileErr != nil {
		m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
	}
	if outFile != nil {
		a.SetLogPath(outFile.Name())
		defer func() { _ = outFile.Close() }()
	}

	var logWriter io.Writer
	if outFile != nil {
		logWriter = outFile
	}

	prompt := cfg.Prompt
	for {
		if !m.runCodexTurn(ctx, a, cfg, prompt, logWriter) {
			return
		}

		a.SetState(StatePaused)
		m.emit(events.AgentState(a.ID), a)

		if cfg.OneShot {
			return
		}

		a.mu.RLock()
		ch := a.promptCh
		a.mu.RUnlock()

		select {
		case <-ctx.Done():
			return
		case next, ok := <-ch:
			if !ok {
				return
			}
			a.SetState(StateRunning)
			m.emit(events.AgentState(a.ID), a)
			prompt = next
		}
	}
}

// runCodexTurn runs one `codex exec --json` process and streams output as
// ConvoEvents. Returns true when turn.completed was observed.
func (m *Manager) runCodexTurn(ctx context.Context, a *Agent, cfg RunConfig, prompt string, logWriter io.Writer) bool {
	args := buildCodexConvoArgs(a, cfg, prompt)
	cmd := exec.CommandContext(ctx, "codex", args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.logger.Error("agent.codex.convo.stdout-pipe", "id", a.ID, "err", err)
		return false
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		m.logger.Error("agent.codex.convo.start", "id", a.ID, "err", err)
		return false
	}
	a.SetCmd(cmd)
	m.logger.Info("agent.codex.convo.turn", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	gotResult := m.streamCodexConvoOutput(a, stdout, logWriter)

	waitErr := cmd.Wait()
	if waitErr != nil {
		m.logger.Error("agent.codex.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
	}
	if s := stderrBuf.String(); s != "" {
		m.logger.Error("agent.codex.convo.stderr", "id", a.ID, "stderr", s)
	}
	return gotResult
}

func buildCodexConvoArgs(a *Agent, cfg RunConfig, prompt string) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check"}
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
	args = append(args, prompt)
	return args
}

func (m *Manager) streamCodexConvoOutput(a *Agent, stdout io.Reader, outFile io.Writer) (gotResult bool) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var lastEmit time.Time
	var pending *ConvoEvent

	for scanner.Scan() {
		line := scanner.Bytes()
		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		// Copy before the scanner reuses the buffer.
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)

		event, err := parseCodexConvoEvent(lineCopy)
		if err != nil {
			m.logger.Warn("agent.codex.convo.parse", "id", a.ID, "err", err, "line", string(lineCopy))
			continue
		}
		if event.Type == "" {
			continue
		}

		a.AppendConvo(event)

		// Always emit result/system events immediately; rate-limit others.
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
			m.logger.Info("agent.codex.convo.result", "id", a.ID, "cost", costNow)
			gotResult = true
		}
	}

	if pending != nil {
		m.emit(events.AgentConvo(a.ID), *pending)
	}
	return gotResult
}

func parseCodexConvoEvent(line []byte) (ConvoEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return ConvoEvent{}, fmt.Errorf("unmarshal: %w", err)
	}

	eventType := strVal(raw, "type")
	event := ConvoEvent{
		Timestamp: time.Now().UTC(),
		Raw:       line,
	}

	switch eventType {
	case "thread.started":
		event.Type = "system"
		event.SessionID = strVal(raw, "thread_id")
	case "turn.started":
		event.Type = "init"
	case "turn.completed":
		usage, _ := raw["usage"].(map[string]any)
		event.Type = "result"
		event.Text = "Completed."
		if usage != nil {
			event.InputTokens = int(floatVal(usage, "input_tokens"))
			event.OutputTokens = int(floatVal(usage, "output_tokens"))
		}
	case "error":
		event.Type = "result"
		event.Subtype = "error"
		event.Text = strVal(raw, "message")
	case "item.started", "item.completed":
		return parseCodexConvoItemEvent(eventType, raw, line)
	default:
		event.Type = eventType
	}
	return event, nil
}

func parseCodexConvoItemEvent(eventType string, raw map[string]any, rawLine []byte) (ConvoEvent, error) {
	item, _ := raw["item"].(map[string]any)
	itemType := strVal(item, "type")

	event := ConvoEvent{Timestamp: time.Now().UTC(), Raw: rawLine}
	switch itemType {
	case "agent_message":
		event.Type = "assistant"
		event.Text = strVal(item, "text")
	case "command_execution":
		if eventType == "item.started" {
			event.Type = "tool_use"
			event.ToolUses = []ToolUseBlock{{
				ID:    strVal(item, "id"),
				Name:  "Bash",
				Input: map[string]any{"command": strVal(item, "command")},
			}}
		} else {
			output := strVal(item, "aggregated_output")
			exitCode := int(floatVal(item, "exit_code"))
			if output == "" {
				output = fmt.Sprintf("Command exited with code %d.", exitCode)
			}
			if len(output) > 2000 {
				output = output[:2000] + "..."
			}
			event.Type = "user"
			event.ToolResults = []ToolResultBlock{{
				ToolUseID: strVal(item, "id"),
				Content:   output,
				IsError:   exitCode != 0,
			}}
		}
	default:
		event.Type = "assistant"
		event.Text = strVal(item, "text")
	}
	return event, nil
}

// sendCodexPrompt delivers a follow-up prompt to a Codex conversational agent.
func (m *Manager) sendCodexPrompt(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}

	a.mu.RLock()
	ch := a.promptCh
	a.mu.RUnlock()
	if ch == nil {
		return fmt.Errorf("agent %s has no prompt channel", agentID)
	}

	// Record user input immediately for the chat UI.
	ev := ConvoEvent{Type: "user_input", Text: text, Timestamp: time.Now().UTC()}
	a.AppendConvo(ev)
	m.emit(events.AgentConvo(a.ID), ev)

	select {
	case ch <- text:
	default:
		return fmt.Errorf("agent %s prompt channel full, a turn may already be in progress", agentID)
	}

	a.SetState(StateRunning)
	m.emit(events.AgentState(a.ID), a)
	m.logger.Info("agent.codex.convo.message_sent", "id", a.ID)
	return nil
}
