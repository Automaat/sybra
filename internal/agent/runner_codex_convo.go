package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	providerpkg "github.com/Automaat/sybra/internal/provider"
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
		m.fireComplete(ctx, a, a.GetExitErr() == nil)
		m.markAgentDone(a)
	}()

	outFile, logWriter := m.openPerTurnConvoLog(a)
	if outFile != nil {
		defer func() { _ = outFile.Close() }()
	}
	m.persistPerTurnConvoSurvival(ctx, a)

	// shutdownSurvive reports whether a ctx cancel should leave the agent
	// running for recreate (survival on, app shutdown, not an intentional stop).
	shutdownSurvive := func() bool {
		return m.survives() && ctx.Err() != nil && !a.WasStopped()
	}

	prompt := cfg.Prompt
	for {
		if !resumeWait {
			updatedCfg, _, regateErr := m.regateForTurn(ctx, a, cfg, logWriter)
			if regateErr != nil {
				m.logger.Warn("agent.convo.regate.blocked", "id", a.ID, "task", a.TaskID, "provider", a.Provider, "err", regateErr)
				a.SetExitErr(errProviderRateLimited)
				select {
				case a.promptChannel() <- prompt:
				default:
				}
				if shutdownSurvive() {
					survived = true
				}
				return
			}
			cfg = updatedCfg

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

		nextPrompt, ok := m.waitPerTurnPrompt(ctx, a)
		if !ok {
			if shutdownSurvive() {
				survived = true
			}
			return
		}
		prompt = nextPrompt
	}
}

// openPerTurnConvoLog reuses an existing recreated-agent log when present and
// otherwise allocates a fresh one, seeding the first segment with the current
// provider marker for mixed-provider rehydration.
func (m *Manager) openPerTurnConvoLog(a *Agent) (*os.File, io.Writer) {
	existingLog := a.GetLogPath()
	var (
		outFile *os.File
		err     error
	)
	if existingLog != "" {
		outFile, err = os.OpenFile(existingLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	} else {
		outFile, err = logging.NewAgentOutputFile(m.logDir, a.ID)
	}
	if err != nil {
		m.logger.Error("agent.output.file", "id", a.ID, "err", err)
		return nil, nil
	}

	a.SetLogPath(outFile.Name())
	if existingLog == "" {
		writeProviderMarkerLine(outFile, a.Provider)
	}
	return outFile, outFile
}

func (m *Manager) persistPerTurnConvoSurvival(ctx context.Context, a *Agent) {
	if !m.survives() {
		return
	}
	a.setDetached(true)
	m.saveRegistry(ctx, a)
}

// waitPerTurnPrompt drains already-queued prompts before blocking on the live
// prompt channel so same-agent handoffs preserve message order.
func (m *Manager) waitPerTurnPrompt(ctx context.Context, a *Agent) (string, bool) {
	if next, ok := a.PopPendingPrompt(); ok {
		a.SetState(StateRunning)
		m.emit(events.AgentState(a.ID), a)
		return next, true
	}

	select {
	case <-ctx.Done():
		return "", false
	case next, ok := <-a.promptChannel():
		if !ok {
			return "", false
		}
		a.SetState(StateRunning)
		m.emit(events.AgentState(a.ID), a)
		return next, true
	}
}

// runConvoTurn runs one per-turn provider process (`codex exec --json` or
// `copilot -p --output-format json`) and streams output as ConvoEvents.
// Returns true only when the turn produced a terminal result and exited cleanly.
func (m *Manager) runConvoTurn(ctx context.Context, a *Agent, cfg RunConfig, prompt string, logWriter io.Writer) bool {
	bin, args, err := buildPerTurnConvoArgs(a, cfg, prompt)
	if err != nil {
		m.logger.Error("agent.convo.provider", "id", a.ID, "provider", a.Provider, "err", err)
		a.SetError("provider", err.Error())
		return false
	}
	cmd := newProviderCmd(ctx, &cfg, false, bin, args...)
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
	attemptEvents := attemptEventsFrom(a.ConvoOutput(), prevLen)
	if streamErr := resultConvoStreamError(attemptEvents); waitErr == nil && streamErr != nil {
		waitErr = streamErr
	}
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "provider", a.Provider, "err", waitErr)
		a.SetExitErr(waitErr)
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	} else {
		a.SetExitErr(nil)
		if m.reportCleanProviderHealthSignalConvo(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit {
			a.SetExitErr(errProviderRateLimited)
		}
	}
	logAttemptStderr(m.logger, "agent.convo.stderr", a.ID, stderrOut, a.GetExitErr(), "provider", a.Provider)
	return gotResult && a.GetExitErr() == nil
}

func resultConvoStreamError(streamEvents []ConvoEvent) error {
	for i := range slices.Backward(streamEvents) {
		e := streamEvents[i]
		if e.Type != "result" {
			continue
		}
		if e.ErrorStatus != 0 || e.ErrorType != "" || resultSubtypeIsError(e.Subtype) {
			if e.ErrorStatus != 0 && e.ErrorType != "" {
				return fmt.Errorf("provider result error %s (%d)", e.ErrorType, e.ErrorStatus)
			}
			if e.ErrorStatus != 0 {
				return fmt.Errorf("provider result error status %d", e.ErrorStatus)
			}
			if e.ErrorType == "" {
				return fmt.Errorf("provider result error")
			}
			return fmt.Errorf("provider result error %s", e.ErrorType)
		}
		return nil
	}
	return nil
}

// buildPerTurnConvoArgs returns the binary name and argv for one per-turn
// conversational turn, dispatching on the agent's provider.
func buildPerTurnConvoArgs(a *Agent, cfg RunConfig, prompt string) (bin string, args []string, err error) {
	prov, err := providerForInvocation(a, cfg)
	if err != nil {
		return "", nil, err
	}
	inv := prov.BuildPerTurnConvoInvocation(a, cfg, prompt)
	return inv.bin, inv.args, nil
}

func buildCodexConvoArgs(a *Agent, cfg RunConfig, prompt string) []string {
	return buildCodexConvoArgsWithProvider(a, cfg, prompt, providerByName("codex"))
}

func buildCodexConvoArgsWithProvider(a *Agent, cfg RunConfig, prompt string, provider Provider) []string {
	skillNames := discoverCodexSkills()
	rewrittenPrompt := rewriteSkillInvocations(prompt, skillNames)
	args := codexExecBaseArgs(rewrittenPrompt != prompt)
	// headless=false: interactive (conversational) mode has a human present
	// who can approve sandbox prompts via the UI.
	args = append(args, provider.SandboxArgs(cfg.RequirePermissions, false)...)
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	args = append(args, codexReasoningArgs(a.ReasoningEffort)...)
	if a.sessionCWD != "" {
		args = append(args, "-C", a.sessionCWD)
	}
	// Lifecycle hooks (fail-open: omitted when sybra-cli unresolvable or taskID unsafe).
	args = append(args, buildCodexHookArgs(a.TaskID)...)
	args = append(args, rewrittenPrompt)
	return args
}

// buildCopilotConvoArgs builds the argv for one Copilot conversational turn.
// Each turn is a non-interactive `copilot -p` run; --no-ask-user keeps it from
// blocking (the user drives clarifications via follow-up prompts). After the
// first turn captures a session id (result event), subsequent turns pass
// --session-id to resume the same conversation.
func buildCopilotConvoArgs(a *Agent, prompt string) []string {
	prompt = stripSkillInvocations(prompt, discoverCopilotSkills())
	args := []string{"-p", prompt, "--output-format", "json", "--allow-all-tools", "--no-ask-user"}
	args = append(args, effortArgs(a.ReasoningEffort)...)
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
// provider-appropriate parser. Only codex/copilot use the per-turn path;
// claude conversational is handled in runner_convo.go.
func parseConvoEvent(provider string, line []byte) (ConvoEvent, error) {
	prov, err := lookupProvider(provider)
	if err != nil {
		return ConvoEvent{}, err
	}
	return prov.ParseConvoLine(line)
}

// convoProviderMarkerVersion identifies the marker line schema. Bump it if a
// field is ever added/changed so old markers (version 0, absent) can still be
// told apart from new ones during rehydration.
const convoProviderMarkerVersion = 1

// convoProviderMarker is a durable, out-of-band log line that records which
// provider's schema parses the lines following it. A mid-run provider switch
// (regateForTurn) writes one at the switch boundary, and a fresh log gets one
// up front recording its starting provider, so rehydratePerTurnConvoFromLog
// can parse each segment of a mixed-provider log with the right parser
// instead of guessing from the agent's current (possibly since-switched)
// provider. The field name is namespaced so it can never collide with a
// genuine codex/copilot JSON line.
type convoProviderMarker struct {
	Marker   string `json:"__sybra_provider_marker__"`
	Version  int    `json:"version"`
	Provider string `json:"provider"`
}

// writeProviderMarkerLine writes a convoProviderMarker line directly to the
// log (bypassing ConvoEvent emission — it is a rehydration aid, not
// conversation content). No-op if w is nil.
func writeProviderMarkerLine(w io.Writer, provider string) {
	if w == nil {
		return
	}
	data, err := json.Marshal(convoProviderMarker{Marker: "provider_switch", Version: convoProviderMarkerVersion, Provider: provider})
	if err != nil {
		return
	}
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
}

// parseProviderMarkerLine reports whether line is a convoProviderMarker and,
// if so, the provider it names. Version is not yet enforced (only version 1
// exists), but is parsed so a future schema change can branch on it.
func parseProviderMarkerLine(line []byte) (string, bool) {
	var mark convoProviderMarker
	if err := json.Unmarshal(line, &mark); err != nil {
		return "", false
	}
	if mark.Marker != "provider_switch" || mark.Provider == "" {
		return "", false
	}
	return mark.Provider, true
}

// rehydratePerTurnConvoFromLog replays a per-turn (codex/copilot)
// conversational agent's log into its convo buffer (and session id) without
// emitting events, so a recreated agent shows its prior chat history after a
// restart. The log may cover more than one provider if a mid-run switch
// occurred; convoProviderMarker lines mark each segment boundary so each
// segment is parsed with its own provider's schema. a.Provider (the agent's
// current provider) is only a fallback, used for any content preceding the
// first marker in an older log written before this mechanism existed.
//
// Session ids are provider-scoped (a Codex thread id means nothing to
// Copilot and vice versa), so the id tracked across the scan resets at every
// marker: a segment must never inherit a session id from the provider that
// preceded it, or the next turn could pass a foreign --session-id to a
// provider that never issued it. Only the final segment's id (the one
// belonging to the agent's current provider) is written to the agent, once,
// after the full scan.
//
// A "claude" segment (a persistent Claude interactive agent that later
// regated to a per-turn peer via regateBeforeClaudeTurn/beginConvoHandoff)
// uses an entirely different line schema (Claude's stream-json, not a
// per-turn provider's ConvoEvent parser), so it is parsed with
// ParseClaudeLine/claudeEventToConvoEvent instead of parseConvoEvent.
func rehydratePerTurnConvoFromLog(a *Agent, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	segmentProvider := a.Provider
	var sessionID string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if p, ok := parseProviderMarkerLine(line); ok {
			segmentProvider = p
			sessionID = ""
			continue
		}
		var ev ConvoEvent
		var perr error
		if segmentProvider == "claude" {
			var parsed ClaudeEvent
			parsed, perr = ParseClaudeLine(line)
			if perr == nil {
				ev = claudeEventToConvoEvent(parsed)
			}
		} else {
			ev, perr = parseConvoEvent(segmentProvider, line)
		}
		if perr != nil {
			continue
		}
		if ev.Type == "" {
			continue
		}
		a.AppendConvo(ev)
		if ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		if ev.Type == "result" {
			a.AddResultStats(ev.SessionID, ev.CostUSD, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens)
			a.AddCacheStats(ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
			if ev.PremiumRequests > 0 {
				a.AddPremiumRequests(ev.PremiumRequests)
			}
		}
	}
	a.SetSessionID(sessionID)
}

// sendConvoPrompt delivers a follow-up prompt to a per-turn (codex/copilot)
// conversational agent via its prompt channel.
func (m *Manager) sendConvoPrompt(agentID, text string) error {
	a, err := m.GetAgent(agentID)
	if err != nil {
		return err
	}

	ch := a.promptChannel()
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
