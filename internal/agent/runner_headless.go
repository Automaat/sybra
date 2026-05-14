package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
)

// headlessEmitInterval caps per-agent stream event emission rate.
// Result events always emit immediately (terminal signal). Frontend
// subscribers may miss intermediate events but can recover via
// GetAgentOutput which reads from outputBuffer.
const headlessEmitInterval = 50 * time.Millisecond

var headlessRetryBackoffs = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}

// headlessScannerBuffer caps the size of a single NDJSON line. A result
// event with a large content field (e.g. a dumped command output) can
// approach this size. Exceeding it aborts the scanner with ErrTooLong and
// is logged below — any regression that lowers this cap will surface in
// stream_tooLong log lines.
const headlessScannerBuffer = 4 * 1024 * 1024

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
	// If CC exited after a graceful SIGINT (WasStopped + session_id captured
	// from the final result event), the next run can pass --resume.
	if a.WasStopped() && a.GetSessionID() != "" {
		a.SetResumable(true)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.headless.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, a.GetExitErr() == nil)
	if m.onComplete != nil {
		m.onComplete(a)
	}
	// Close `done` only after onComplete returns. HasRunningAgentForTask
	// gates ResumeStalled; releasing it before the workflow advance handler
	// runs lets a tight ResumeStalled loop dispatch a duplicate agent.
	m.markAgentDone(a)
}

func (m *Manager) runHeadlessAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File) (retry bool, err error) {
	if outFile == nil {
		return false, nil
	}
	name, args, invokeEnv, command, err := buildHeadlessInvocation(a, cfg)
	if err != nil {
		return false, err
	}

	cmd := exec.CommandContext(ctx, name, args...)
	configureGracefulShutdown(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 || len(invokeEnv) > 0 {
		cmd.Env = append(os.Environ(), invokeEnv...)
		cmd.Env = append(cmd.Env, cfg.ExtraEnv...)
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

		if (event.Type == "system" || event.Type == "init") && len(event.PluginErrors) > 0 {
			for _, e := range event.PluginErrors {
				m.logger.Warn("agent.plugin_error", "id", a.ID, "error", e)
			}
			a.SetPluginErrors(event.PluginErrors)
			m.emit(events.AgentPluginErrors(a.ID), PluginErrorsEvent{Errors: event.PluginErrors})
			m.emit(events.AgentState(a.ID), a)
		}

		if event.Type == "assistant" {
			if keepGoing := m.checkTurnsGuardrail(ctx, a); !keepGoing {
				return
			}
		}

		if event.Type == "result" {
			costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
			a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
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

// checkTurnsGuardrail increments the turn counter and, if the limit is
// reached, either auto-continues (cost below threshold) or blocks until a
// human decision arrives. Returns false if the caller should stop the stream.
func (m *Manager) checkTurnsGuardrail(ctx context.Context, a *Agent) bool {
	turns := a.IncTurnCount()
	maxTurns := m.effectiveMaxTurns(a)
	if maxTurns <= 0 || turns < maxTurns {
		return true
	}
	m.logger.Warn("agent.guardrail.turns", "id", a.ID, "turns", turns, "limit", maxTurns)
	if m.canAutoContinueTurns(a) {
		multiplier := m.effectiveTurnMultiplier()
		newLimit := int(float64(maxTurns) * multiplier)
		a.SetMaxTurns(newLimit)
		m.logger.Info("agent.guardrail.turns.auto_continued", "id", a.ID, "turns", turns, "new_limit", newLimit)
		return true
	}
	a.SetEscalationReason("turns")
	m.emit(events.AgentEscalation(a.ID), EscalationEvent{
		Reason:    "turns",
		TurnCount: turns,
		Limit:     float64(maxTurns),
	})
	m.emit(events.AgentState(a.ID), a)
	select {
	case continueRun := <-a.escalationCh:
		if !continueRun {
			a.cancel()
			return false
		}
		a.SetEscalationReason("")
		m.emit(events.AgentState(a.ID), a)
		return true
	case <-ctx.Done():
		return false
	}
}

// effectiveMaxTurns returns the turn limit for a: per-agent override when set,
// otherwise the global guardrail.
func (m *Manager) effectiveMaxTurns(a *Agent) int {
	m.mu.RLock()
	global := m.guardrails.MaxTurns
	m.mu.RUnlock()
	a.mu.RLock()
	perAgent := a.MaxTurns
	a.mu.RUnlock()
	if perAgent > 0 {
		return perAgent
	}
	return global
}

func (m *Manager) handleError(a *Agent, err error) {
	kind := classifyAgentError(err)
	a.SetError(kind, err.Error())
	a.SetState(StateStopped)
	m.logger.Error("agent.error", "id", a.ID, "kind", kind, "err", err)
	m.emit(events.AgentError(a.ID), ErrorEvent{Kind: kind, Msg: err.Error()})
	m.emit(events.AgentState(a.ID), a)
	m.recordCompletion(a, false)
	if m.onComplete != nil {
		m.onComplete(a)
	}
	m.markAgentDone(a)
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
