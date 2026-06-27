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
		Type:          e.Type,
		Subtype:       e.Subtype,
		SessionID:     e.SessionID,
		Timestamp:     time.Now().UTC(),
		Raw:           e.Raw,
		LimitSnapshot: e.Limits,
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
			ev.CacheCreationInputTokens = e.Result.CacheCreationInputTokens
			ev.CacheReadInputTokens = e.Result.CacheReadInputTokens
			ev.ReasoningTokens = e.Result.ReasoningTokens
			ev.ErrorType = e.Result.ErrorType
			ev.ErrorStatus = e.Result.ErrorStatus
		}
	}
	return ev
}

// copilotEventToConvoEvent converts a parsed CopilotEvent into a ConvoEvent
// for conversational mode. Mirrors codexEventToConvoEvent: "tool_result" maps
// to type "user" (Claude stream-json convention) and tool result content is
// truncated to 2000 chars. Copilot reports premium-request usage and
// per-message output tokens rather than USD cost.
func copilotEventToConvoEvent(e CopilotEvent) ConvoEvent {
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
		}
		// Deliberately omit ToolUses here: Copilot emits a separate
		// tool.execution_start event (mapped to a tool_use ConvoEvent) for the
		// same tool call, so copying the assistant.message toolRequests too
		// would render every tool twice in the timeline.
		ev.OutputTokens = e.OutputTokens
	case "tool_use":
		if e.Message != nil {
			ev.ToolUses = e.Message.ToolUses
		}
	case "tool_result":
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
			ev.PremiumRequests = e.Result.PremiumRequests
			ev.ErrorType = e.Result.ErrorType
			ev.ErrorStatus = e.Result.ErrorStatus
		}
	}
	return ev
}

// runPerTurnConversational runs a per-turn provider (codex/copilot) in
// interactive conversational mode. Each turn spawns a fresh process
// (`codex exec --json` or `copilot -p --output-format json`). After the turn's
// terminal result the agent transitions to StatePaused and waits for the next
// prompt on promptCh. OneShot skips the wait and exits after the first turn.
//
// resumeWait starts the loop already idle (skip the first turn, wait for the
// next prompt) — used when reattaching a recreated agent after a restart.
//
// Restart survival: per-turn convo has no persistent process between turns, so
// it "survives" by recreate-on-restart. When survival is on, the agent is
// recorded; on a shutdown (ctx cancel that is NOT an intentional stop) the
// goroutine returns WITHOUT finalizing, leaving the record for the next
// startup to recreate. A normal completion / user-close finalizes and
// deletes the record.
func (m *Manager) runPerTurnConversational(ctx context.Context, a *Agent, cfg RunConfig, resumeWait bool) {
	survived := false
	defer func() {
		if survived {
			m.logger.Info("agent.convo.detach", "id", a.ID, "provider", a.Provider, "reason", "shutdown")
			return
		}
		a.SetState(StateStopped)
		m.logger.Info("agent.convo.done", "id", a.ID, "provider", a.Provider, "cost", a.GetCostUSD())
		m.emit(events.AgentState(a.ID), a)
		m.fireComplete(a, a.GetExitErr() == nil)
		m.markAgentDone(a)
	}()

	// On recreate (resume), append to the existing log so the rehydrated
	// chat history is preserved across restarts; a fresh run opens a new
	// file. Without this, each restart would open a new empty log and the
	// next restart would rehydrate zero history.
	var outFile *os.File
	var fileErr error
	if existing := a.GetLogPath(); existing != "" {
		outFile, fileErr = os.OpenFile(existing, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	} else {
		outFile, fileErr = logging.NewAgentOutputFile(m.logDir, a.ID)
	}
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

	// Record the agent so a restart can recreate it (the log path is now set).
	if m.survives() {
		a.setDetached(true)
		m.saveRegistry(a)
	}

	// shutdownSurvive reports whether a ctx cancel should leave the agent
	// running for recreate (survival on, app shutdown, not an intentional stop).
	shutdownSurvive := func() bool {
		return m.survives() && ctx.Err() != nil && !a.WasStopped()
	}

	prompt := cfg.Prompt
	for {
		if !resumeWait {
			if !m.runConvoTurn(ctx, a, cfg, prompt, logWriter) {
				if shutdownSurvive() {
					survived = true
				}
				return
			}

			a.SetState(StatePaused)
			m.emit(events.AgentState(a.ID), a)

			if cfg.OneShot {
				return
			}
		}
		resumeWait = false

		a.mu.RLock()
		ch := a.promptCh
		a.mu.RUnlock()

		select {
		case <-ctx.Done():
			if shutdownSurvive() {
				survived = true
			}
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

// runConvoTurn runs one per-turn provider process (`codex exec --json` or
// `copilot -p --output-format json`) and streams output as ConvoEvents.
// Returns true when the turn's terminal result was observed.
func (m *Manager) runConvoTurn(ctx context.Context, a *Agent, cfg RunConfig, prompt string, logWriter io.Writer) bool {
	bin, args := buildPerTurnConvoArgs(a, cfg, prompt)
	cmd := exec.CommandContext(ctx, bin, args...)
	configureGracefulShutdown(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.logger.Error("agent.convo.stdout-pipe", "id", a.ID, "provider", a.Provider, "err", err)
		return false
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		m.logger.Error("agent.convo.start", "id", a.ID, "provider", a.Provider, "err", err)
		return false
	}
	a.SetCmd(cmd)
	m.logger.Info("agent.convo.turn", "id", a.ID, "provider", a.Provider, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	prevLen := len(a.ConvoOutput())
	gotResult := m.streamPerTurnConvoOutput(a, stdout, logWriter)

	waitErr := cmd.Wait()
	stderrOut := stderrBuf.String()
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "provider", a.Provider, "err", waitErr)
		a.SetExitErr(waitErr)
		all := a.ConvoOutput()
		if prevLen > len(all) {
			prevLen = len(all)
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, all[prevLen:])
	}
	if stderrOut != "" {
		m.logger.Error("agent.convo.stderr", "id", a.ID, "provider", a.Provider, "stderr", stderrOut)
	}
	return gotResult
}

// buildPerTurnConvoArgs returns the binary name and argv for one per-turn
// conversational turn, dispatching on the agent's provider.
func buildPerTurnConvoArgs(a *Agent, cfg RunConfig, prompt string) (bin string, args []string) {
	if normalizeProvider(a.Provider) == "copilot" {
		return "copilot", buildCopilotConvoArgs(a, prompt)
	}
	return "codex", buildCodexConvoArgs(a, cfg, prompt)
}

func buildCodexConvoArgs(a *Agent, cfg RunConfig, prompt string) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}
	// headless=false: interactive (conversational) mode has a human present
	// who can approve sandbox prompts via the UI.
	args = append(args, codexSandboxArgs(cfg.RequirePermissions, false)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = append(args, codexReasoningArgs(a.ReasoningEffort)...)
	if a.sessionCWD != "" {
		args = append(args, "-C", a.sessionCWD)
	}
	// Lifecycle hooks (fail-open: omitted when sybra-cli unresolvable or taskID unsafe).
	args = append(args, buildCodexHookArgs(a.TaskID)...)
	args = append(args, rewriteSkillInvocations(prompt, discoverCodexSkills()))
	return args
}

// buildCopilotConvoArgs builds the argv for one Copilot conversational turn.
// Each turn is a non-interactive `copilot -p` run; --no-ask-user keeps it from
// blocking (the user drives clarifications via follow-up prompts). After the
// first turn captures a session id (result event), subsequent turns pass
// --session-id to resume the same conversation.
func buildCopilotConvoArgs(a *Agent, prompt string) []string {
	args := []string{"-p", prompt, "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if sid := a.GetSessionID(); sid != "" {
		args = append(args, "--session-id", sid)
	}
	return args
}

func (m *Manager) streamPerTurnConvoOutput(a *Agent, stdout io.Reader, outFile io.Writer) (gotResult bool) {
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

		// parseConvoEvent copies the scanner buffer internally so no manual copy needed.
		event, parseErr := parseConvoEvent(a.Provider, line)
		if parseErr != nil {
			m.logger.Warn("agent.convo.parse", "id", a.ID, "provider", a.Provider, "err", parseErr, "line", string(line))
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
		case "usage":
			if event.LimitSnapshot != nil {
				snapshot := *event.LimitSnapshot
				if snapshot.Provider == "" {
					snapshot.Provider = a.Provider
				}
				m.mu.RLock()
				limitSink := m.limitSink
				m.mu.RUnlock()
				if limitSink != nil {
					limitSink(snapshot)
				}
			}
		case "system":
			if event.SessionID != "" {
				a.SetSessionID(event.SessionID)
				if p := resolveCodexSessionFile(event.SessionID); p != "" {
					a.SetSessionFilePath(p)
				}
			}
		case "assistant":
			// Copilot reports output tokens per assistant message; accumulate
			// (no-op for codex, which carries 0 here and totals on result).
			if event.OutputTokens > 0 {
				a.AddOutputTokens(event.OutputTokens)
			}
		case "result":
			// Copilot reports its session id only on the terminal result event
			// (codex reports it on a system event handled above). Capture it so
			// the next turn resumes via --session-id.
			if event.SessionID != "" {
				a.SetSessionID(event.SessionID)
			}
			costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
			a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
			if event.PremiumRequests > 0 {
				a.AddPremiumRequests(event.PremiumRequests)
			}
			m.logger.Info("agent.convo.result", "id", a.ID, "provider", a.Provider, "cost", costNow)
			gotResult = true
		}
	}

	if pending != nil {
		m.emit(events.AgentConvo(a.ID), *pending)
	}
	return gotResult
}

// parseConvoEvent parses one per-turn NDJSON line into a ConvoEvent using the
// provider-appropriate parser. Only codex/copilot use the per-turn path
// (claude conversational is handled in runner_convo.go), so the default is codex.
func parseConvoEvent(provider string, line []byte) (ConvoEvent, error) {
	if normalizeProvider(provider) == "copilot" {
		ce, err := ParseCopilotLine(line)
		if err != nil {
			return ConvoEvent{}, err
		}
		return copilotEventToConvoEvent(ce), nil
	}
	ce, err := ParseCodexLine(line)
	if err != nil {
		return ConvoEvent{}, err
	}
	return codexEventToConvoEvent(ce), nil
}

// rehydratePerTurnConvoFromLog replays a per-turn (codex/copilot)
// conversational agent's log into its convo buffer (and session id) without
// emitting events, so a recreated agent shows its prior chat history after a
// restart.
func rehydratePerTurnConvoFromLog(a *Agent, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, perr := parseConvoEvent(a.Provider, line)
		if perr != nil {
			continue
		}
		if ev.Type == "" {
			continue
		}
		a.AppendConvo(ev)
		if ev.SessionID != "" {
			a.SetSessionID(ev.SessionID)
		}
		if ev.Type == "result" {
			a.AddResultStats(ev.SessionID, ev.CostUSD, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens)
			a.AddCacheStats(ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
			if ev.PremiumRequests > 0 {
				a.AddPremiumRequests(ev.PremiumRequests)
			}
		}
	}
}

// sendConvoPrompt delivers a follow-up prompt to a per-turn (codex/copilot)
// conversational agent via its prompt channel.
func (m *Manager) sendConvoPrompt(agentID, text string) error {
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
	m.logger.Info("agent.convo.message_sent", "id", a.ID, "provider", a.Provider)
	return nil
}
