package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
)

// codexEventToConvoEvent converts a shared CodexEvent into a ConvoEvent for
// conversational mode. Tool result content is truncated to 2000 chars.
// "tool_result" CodexEvent type maps to ConvoEvent type "user" to match the
// Claude stream-json convention used by the frontend.
func codexEventToConvoEvent(e CodexEvent) ConvoEvent {
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
	case "tool_use":
		if e.Message != nil {
			ev.ToolUses = e.Message.ToolUses
		}
	case "tool_result":
		// Map to "user" to match Claude stream-json convention.
		ev.Type = "user"
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
			ev.ErrorType = e.Result.ErrorType
			ev.ErrorStatus = e.Result.ErrorStatus
		}
	}
	return ev
}

// runCodexConversational runs Codex in interactive conversational mode.
// Each turn spawns a fresh `codex exec --json` process. After turn.completed
// the agent transitions to StatePaused and waits for the next prompt on
// promptCh. OneShot skips the wait and exits after the first turn.
func (m *Manager) runCodexConversational(ctx context.Context, a *Agent, cfg RunConfig) {
	defer func() {
		a.SetState(StateStopped)
		m.markAgentDone(a)
		m.logger.Info("agent.codex.convo.done", "id", a.ID, "cost", a.GetCostUSD())
		m.emit(events.AgentState(a.ID), a)
		m.recordCompletion(a, a.GetExitErr() == nil)
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
	configureGracefulShutdown(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
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

	prevLen := len(a.ConvoOutput())
	gotResult := m.streamCodexConvoOutput(a, stdout, logWriter)

	waitErr := cmd.Wait()
	stderrOut := stderrBuf.String()
	if waitErr != nil {
		m.logger.Error("agent.codex.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
		all := a.ConvoOutput()
		if prevLen > len(all) {
			prevLen = len(all)
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, all[prevLen:])
	}
	if stderrOut != "" {
		m.logger.Error("agent.codex.convo.stderr", "id", a.ID, "stderr", stderrOut)
	}
	return gotResult
}

func buildCodexConvoArgs(a *Agent, cfg RunConfig, prompt string) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	args = append(args, codexSandboxArgs(cfg.RequirePermissions)...)
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

		// ParseCodexLine copies the scanner buffer internally so no manual copy needed.
		parsed, parseErr := ParseCodexLine(line)
		if parseErr != nil {
			m.logger.Warn("agent.codex.convo.parse", "id", a.ID, "err", parseErr, "line", string(line))
			continue
		}
		event := codexEventToConvoEvent(parsed)
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
				if p := resolveCodexSessionFile(event.SessionID); p != "" {
					a.SetSessionFilePath(p)
				}
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
