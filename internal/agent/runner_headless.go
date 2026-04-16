package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/provider"
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
	m.markAgentDone(a)
	m.logger.Info("agent.headless.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, a.GetExitErr() == nil)
	m.callOnComplete(a)
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
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
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
		attemptEvents := all[prevLen:]
		if shouldRetry(stderrOut, attemptEvents, m.logger) {
			return true, nil
		}
		m.reportProviderHealthSignal(a, stderrOut, attemptEvents)
	}
	return false, nil
}

// reportProviderHealthSignal classifies the final error surface of a failed
// run and forwards rate-limit / auth failures to the provider health gate so
// the next scheduling attempt can fail over to a peer.
func (m *Manager) reportProviderHealthSignal(a *Agent, stderrOut string, attemptEvents []StreamEvent) {
	sample := buildErrorSample(stderrOut, attemptEvents)
	var sig provider.Signal
	var reason string
	var retryAfter time.Duration
	if a.Provider == "codex" {
		sig, reason, retryAfter = provider.ClassifyCodexError(sample)
	} else {
		sig, reason, retryAfter = provider.ClassifyClaudeError(sample)
	}
	if sig == provider.SignalNone {
		if sample.ErrorType != "" || sample.ErrorStatus != 0 {
			m.logger.Info("agent.provider.signal.unknown",
				"provider", a.Provider,
				"errorType", sample.ErrorType,
				"errorStatus", sample.ErrorStatus)
		}
		return
	}
	m.ReportProviderSignal(a.Provider, sig, reason, retryAfter)
}

func buildErrorSample(stderrOut string, attemptEvents []StreamEvent) provider.ErrorSample {
	sample := provider.ErrorSample{Stderr: stderrOut}
	for i := len(attemptEvents) - 1; i >= 0; i-- {
		e := attemptEvents[i]
		if e.Type != "result" || e.Subtype != "error" {
			continue
		}
		sample.ErrorType = e.ErrorType
		sample.ErrorStatus = e.ErrorStatus
		sample.Content = e.Content
		break
	}
	return sample
}

// reportProviderHealthSignalConvo mirrors reportProviderHealthSignal for the
// ConvoEvent stream used by conversational runners.
func (m *Manager) reportProviderHealthSignalConvo(a *Agent, stderrOut string, attemptEvents []ConvoEvent) {
	sample := buildErrorSampleConvo(stderrOut, attemptEvents)
	var sig provider.Signal
	var reason string
	var retryAfter time.Duration
	if a.Provider == "codex" {
		sig, reason, retryAfter = provider.ClassifyCodexError(sample)
	} else {
		sig, reason, retryAfter = provider.ClassifyClaudeError(sample)
	}
	if sig == provider.SignalNone {
		if sample.ErrorType != "" || sample.ErrorStatus != 0 {
			m.logger.Info("agent.provider.signal.unknown",
				"provider", a.Provider,
				"errorType", sample.ErrorType,
				"errorStatus", sample.ErrorStatus)
		}
		return
	}
	m.ReportProviderSignal(a.Provider, sig, reason, retryAfter)
}

func buildErrorSampleConvo(stderrOut string, attemptEvents []ConvoEvent) provider.ErrorSample {
	sample := provider.ErrorSample{Stderr: stderrOut}
	for i := len(attemptEvents) - 1; i >= 0; i-- {
		e := attemptEvents[i]
		if e.Type != "result" || e.Subtype != "error" {
			continue
		}
		sample.ErrorType = e.ErrorType
		sample.ErrorStatus = e.ErrorStatus
		sample.Content = e.Text
		break
	}
	return sample
}

// shouldRetry returns true when stderrOut or streamEvents indicate an Anthropic
// 529 (overloaded) transient error that warrants a backoff retry.
//
// Structured fields on StreamEvent (ErrorType, ErrorStatus) are checked first.
// Substring matching is used as a fallback and triggers a Warn log so format
// regressions surface in logs without silently breaking retries.
func shouldRetry(stderrOut string, streamEvents []StreamEvent, logger *slog.Logger) bool {
	for i := range streamEvents {
		if streamEvents[i].Type == "result" && streamEvents[i].Subtype == "error" {
			if streamEvents[i].ErrorType == "overloaded_error" || streamEvents[i].ErrorStatus == 529 {
				return true
			}
		}
	}
	// Substring fallback: keeps working if Anthropic changes the error envelope.
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	for i := range streamEvents {
		if streamEvents[i].Type == "result" && streamEvents[i].Subtype == "error" && substringMatch529(streamEvents[i].Content) {
			warnSubstringFallback(logger)
			return true
		}
	}
	return false
}

// shouldRetryConvo is the ConvoEvent variant of shouldRetry.
func shouldRetryConvo(stderrOut string, convoEvents []ConvoEvent, logger *slog.Logger) bool {
	for i := range convoEvents {
		e := &convoEvents[i]
		if e.Type == "result" && e.Subtype == "error" {
			if e.ErrorType == "overloaded_error" || e.ErrorStatus == 529 {
				return true
			}
		}
	}
	if substringMatch529(stderrOut) {
		warnSubstringFallback(logger)
		return true
	}
	for i := range convoEvents {
		e := &convoEvents[i]
		if e.Type == "result" && e.Subtype == "error" && substringMatch529(e.Text) {
			warnSubstringFallback(logger)
			return true
		}
	}
	return false
}

func substringMatch529(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "529") || strings.Contains(lower, "overloaded")
}

func warnSubstringFallback(logger *slog.Logger) {
	if logger != nil {
		logger.Warn("agent.retry.substring-fallback",
			"hint", "structured error fields absent; check if Anthropic changed error format")
	}
}

// trackingReader wraps an io.Reader and calls touch on every Read, keeping
// LastEventAt alive during extended thinking where no complete NDJSON lines
// are emitted for several minutes.
type trackingReader struct {
	r     io.Reader
	touch func()
}

func (t *trackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

// headlessScannerBuffer caps the size of a single NDJSON line. A result
// event with a large content field (e.g. a dumped command output) can
// approach this size. Exceeding it aborts the scanner with ErrTooLong and
// is logged below — any regression that lowers this cap will surface in
// stream_tooLong log lines.
const headlessScannerBuffer = 4 * 1024 * 1024

func (m *Manager) streamHeadlessOutput(ctx context.Context, a *Agent, stdout io.Reader, outFile io.Writer) {
	tracked := &trackingReader{r: stdout, touch: a.TouchLastEvent}
	scanner := bufio.NewScanner(tracked)
	scanner.Buffer(make([]byte, 0, headlessScannerBuffer), headlessScannerBuffer)
	var lastEmit time.Time
	isCodex := normalizeProvider(a.Provider) == "codex"
	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		var event StreamEvent
		var parseErr error
		if isCodex {
			var ce CodexEvent
			ce, parseErr = ParseCodexLine(line)
			if parseErr == nil {
				event = codexEventToStreamEvent(ce)
			}
		} else {
			var ce ClaudeEvent
			ce, parseErr = ParseClaudeLine(line)
			if parseErr == nil {
				event = claudeEventToStreamEvent(ce)
			}
		}
		if parseErr != nil {
			m.logger.Warn("agent.headless.parse", "id", a.ID, "err", parseErr, "line", string(line))
			continue
		}
		if event.Type == "" {
			continue
		}

		event.Timestamp = time.Now().UTC()
		a.AppendOutput(event)
		if event.Type == "result" || time.Since(lastEmit) >= headlessEmitInterval {
			m.emit(events.AgentOutput(a.ID), event)
			lastEmit = time.Now()
		}

		if event.Type == "init" && event.SessionID != "" && a.Provider == "codex" {
			if p := resolveCodexSessionFile(event.SessionID); p != "" {
				a.SetSessionFilePath(p)
			}
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
	m.reportScannerError(a, scanner.Err())
}

// reportScannerError surfaces bufio.Scanner errors at the end of the NDJSON
// loop so oversized lines and pipe failures don't silently drop events.
// io.EOF is the normal exit path and never reaches this function.
func (m *Manager) reportScannerError(a *Agent, err error) {
	if err == nil {
		return
	}
	m.logger.Warn("agent.headless.stream.error",
		"id", a.ID,
		"err", err,
		"hint", "oversized line or broken pipe aborted the NDJSON stream; trailing events were lost")
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

// claudeEventToStreamEvent converts a shared ClaudeEvent into a StreamEvent
// for the headless runner. Tool uses are formatted as "[name] cmd/desc" strings.
// Tool results are truncated to 500 chars.
func claudeEventToStreamEvent(e ClaudeEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Content = formatHeadlessAssistant(e.Message)
			ev.PlanSteps = extractTodoWriteSteps(e.Message.ToolUses)
		}
	case "user":
		if e.Message != nil {
			ev.Content = formatHeadlessToolResults(e.Message.ToolResults)
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
			ev.SessionID = e.Result.SessionID
			ev.CostUSD = e.Result.CostUSD
			ev.InputTokens = e.Result.InputTokens
			ev.OutputTokens = e.Result.OutputTokens
		}

	}
	return ev
}

// extractTodoWriteSteps scans tool uses for a TodoWrite call and returns the
// parsed todo list. Returns nil if no TodoWrite call is present or parsing fails.
func extractTodoWriteSteps(toolUses []ToolUseBlock) []PlanStep {
	for i := range toolUses {
		if toolUses[i].Name != "TodoWrite" {
			continue
		}
		todosRaw, ok := toolUses[i].Input["todos"]
		if !ok {
			return nil
		}
		items, ok := todosRaw.([]any)
		if !ok {
			return nil
		}
		steps := make([]PlanStep, 0, len(items))
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, _ := m["content"].(string)
			status, _ := m["status"].(string)
			if content == "" {
				continue
			}
			steps = append(steps, PlanStep{Content: content, Status: status})
		}
		return steps
	}
	return nil
}

// codexEventToStreamEvent converts a shared CodexEvent into a StreamEvent
// for the headless runner.
func codexEventToStreamEvent(e CodexEvent) StreamEvent {
	ev := StreamEvent{Type: e.Type, Subtype: e.Subtype, SessionID: e.SessionID}
	switch e.Type {
	case "assistant":
		if e.Message != nil {
			ev.Content = e.Message.Text
		}
	case "tool_use":
		if e.Message != nil && len(e.Message.ToolUses) > 0 {
			cmd, _ := e.Message.ToolUses[0].Input["command"].(string)
			ev.Content = cmd
		}
	case "tool_result":
		if e.Message != nil && len(e.Message.ToolResults) > 0 {
			ev.Content = e.Message.ToolResults[0].Content
		}
	case "result":
		if e.Result != nil {
			ev.Content = e.Result.Text
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

// formatHeadlessAssistant produces the flat content string for headless assistant
// events: joined text parts followed by "[name] cmd/desc" tool use lines.
func formatHeadlessAssistant(msg *ClaudeMessage) string {
	var parts []string
	if msg.Text != "" {
		parts = append(parts, msg.Text)
	}
	for _, tu := range msg.ToolUses {
		if tu.Input == nil {
			parts = append(parts, fmt.Sprintf("[%s]", tu.Name))
			continue
		}
		desc, _ := tu.Input["description"].(string)
		cmd, _ := tu.Input["command"].(string)
		switch {
		case desc != "":
			parts = append(parts, fmt.Sprintf("[%s] %s", tu.Name, desc))
		case cmd != "":
			parts = append(parts, fmt.Sprintf("[%s] %s", tu.Name, cmd))
		default:
			parts = append(parts, fmt.Sprintf("[%s]", tu.Name))
		}
	}
	return strings.Join(parts, "\n")
}

// formatHeadlessToolResults joins tool result contents, truncating each to 500 chars.
func formatHeadlessToolResults(results []ToolResultBlock) string {
	var parts []string
	for _, tr := range results {
		content := tr.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		if content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n")
}

func (m *Manager) handleError(a *Agent, err error) {
	kind := classifyAgentError(err)
	a.SetError(kind, err.Error())
	a.SetState(StateStopped)
	m.markAgentDone(a)
	m.logger.Error("agent.error", "id", a.ID, "kind", kind, "err", err)
	m.emit(events.AgentError(a.ID), ErrorEvent{Kind: kind, Msg: err.Error()})
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, false)
	m.callOnComplete(a)
}

// classifyAgentError maps a fatal agent error to a canonical kind string.
func classifyAgentError(err error) string {
	if err == nil {
		return "crash"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "worktree") || strings.Contains(msg, "already checked out"):
		return "worktree_conflict"
	case strings.Contains(msg, "clone") ||
		strings.Contains(msg, "fetch origin") ||
		strings.Contains(msg, "git fetch") ||
		strings.Contains(msg, "could not resolve host") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "dns") ||
		(strings.Contains(msg, "git") && strings.Contains(msg, "network")):
		return "git_clone"
	case strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "eacces") ||
		strings.Contains(msg, "operation not permitted"):
		return "permission_denied"
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429") || strings.Contains(msg, "overloaded"):
		return "rate_limit"
	default:
		return "crash"
	}
}
