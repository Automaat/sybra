package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
)

// errSurviveShutdown is returned by a detached headless attempt when the
// app's context is cancelled (shutdown) while the subprocess is still
// alive. The runner returns without finalizing so the agent is left
// running for the next instance to reattach.
var errSurviveShutdown = errors.New("agent: detached, leaving process running for reattach")

// headlessTailPoll is how often the detached/reattached tailer polls the
// log file for new NDJSON lines.
const headlessTailPoll = 100 * time.Millisecond

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

	// tailOffset tracks how far the detached tailer has consumed the shared
	// log file, so a retry resumes after the prior attempt's lines instead
	// of reprocessing them. Unused by the legacy pipe path.
	var tailOffset int64

	for attempt := range len(headlessRetryBackoffs) + 1 {
		if attempt > 0 {
			wait := headlessRetryBackoffs[attempt-1]
			m.logger.Info("agent.headless.retry", "id", a.ID, "attempt", attempt, "backoff", wait)
			select {
			case <-ctx.Done():
				// App shutdown between retries on a detached agent: the prior
				// process is already gone, so leave finalize to the next
				// reattach rather than advancing a workflow mid-shutdown.
				if a.isDetached() && !a.WasStopped() {
					return
				}
				goto done
			case <-time.After(wait):
			}
		}

		retry, fatalErr := m.runHeadlessAttempt(ctx, a, cfg, &outFile, &tailOffset)
		if errors.Is(fatalErr, errSurviveShutdown) {
			// Detached subprocess left running across shutdown — do not
			// finalize; the next app instance reattaches via the registry.
			return
		}
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
	m.fireComplete(a, a.GetExitErr() == nil)
	// Close `done` only after onComplete returns. HasRunningAgentForTask
	// gates ResumeStalled; releasing it before the workflow advance handler
	// runs lets a tight ResumeStalled loop dispatch a duplicate agent.
	m.markAgentDone(a)
}

func (m *Manager) runHeadlessAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64) (retry bool, err error) {
	if outFile == nil {
		return false, nil
	}

	// Materialize the output schema to a temp file for codex so
	// buildHeadlessInvocation can pass --output-schema <path>. The file is
	// removed after cmd.Wait() returns (subprocess has exited), so there is no
	// read-after-delete risk. os.CreateTemp gives a unique name per attempt so
	// concurrent agents or retries never collide.
	if normalizeProvider(a.Provider) == "codex" && cfg.OutputSchema != "" {
		f, schemaErr := os.CreateTemp("", "sybra-codex-schema-*.json")
		if schemaErr != nil {
			return false, fmt.Errorf("create codex output schema: %w", schemaErr)
		}
		if _, wErr := f.WriteString(cfg.OutputSchema); wErr != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return false, fmt.Errorf("write codex output schema: %w", wErr)
		}
		_ = f.Close()
		cfg.outputSchemaPath = f.Name()
		defer os.Remove(cfg.outputSchemaPath)
	}

	name, args, invokeEnv, command, err := buildHeadlessInvocation(a, cfg)
	if err != nil {
		return false, err
	}

	if m.survives() && a.Mode == "headless" {
		return m.runHeadlessAttemptSurvive(ctx, a, cfg, outFile, tailOffset, name, args, invokeEnv, command)
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

// runHeadlessAttemptSurvive spawns a detached headless subprocess whose
// stdout is the NDJSON log file (not a pipe), so it survives the parent's
// exit and a terminal Ctrl-C. The manager reads output by tailing that
// file. Returns errSurviveShutdown if the app shuts down while the child
// is still alive — the caller then leaves it running for reattach.
func (m *Manager) runHeadlessAttemptSurvive(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64, name string, args, invokeEnv []string, command string) (retry bool, err error) {
	// The log file is the child's stdout, so it must exist before Start.
	// Opened once and reused across retries (append mode).
	if *outFile == nil {
		f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
		if fileErr != nil {
			return false, fmt.Errorf("open agent log: %w", fileErr)
		}
		a.SetLogPath(f.Name())
		*outFile = f
	}
	logPath := (*outFile).Name()

	cmd := exec.Command(name, args...) // no Context: a cancelled ctx must not kill a detached child
	configureDetached(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 || len(invokeEnv) > 0 {
		cmd.Env = append(os.Environ(), invokeEnv...)
		cmd.Env = append(cmd.Env, cfg.ExtraEnv...)
	}
	a.Command = command
	cmd.Stdout = *outFile

	stderrPath := logPath + ".stderr"
	if stderrF, ferr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); ferr == nil {
		cmd.Stderr = stderrF
		defer func() { _ = stderrF.Close() }()
	}

	if startErr := cmd.Start(); startErr != nil {
		return false, fmt.Errorf("start %s: %w", name, startErr)
	}
	a.SetCmd(cmd)
	a.setDetached(true)
	m.saveRegistry(a)
	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir, "detached", true)

	procDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(procDone)
	}()

	prevLen := len(a.Output())
	startOffset := int64(0)
	if tailOffset != nil {
		startOffset = *tailOffset
	}
	exited, endOffset := m.tailHeadlessFile(ctx, a, logPath, startOffset, procDone)
	if tailOffset != nil {
		*tailOffset = endOffset
	}
	if !exited {
		m.logger.Info("agent.headless.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown")
		return false, errSurviveShutdown
	}

	// Only inspect exit status / retry once the process has actually exited.
	// A guardrail kill or intentional stop can return from the tailer after
	// drainTimeout with the wait goroutine still running; reading waitErr
	// then would race the write and misread a live process as a clean exit.
	select {
	case <-procDone:
	default:
		return false, nil
	}

	var stderrOut string
	if b, readErr := os.ReadFile(stderrPath); readErr == nil {
		stderrOut = string(b)
	}
	if stderrOut != "" {
		m.logger.Error("agent.headless.stderr", "id", a.ID, "stderr", stderrOut)
	}
	if waitErr != nil {
		m.logger.Error("agent.headless.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
		// Only inspect events from this attempt, mirroring the legacy path —
		// otherwise a transient 529 from an earlier attempt makes every later
		// attempt retry regardless of its real failure.
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

// drainTimeout bounds how long the tailer waits for a process it just
// asked to stop (guardrail kill or StopAgent) to actually exit before it
// gives up and finalizes anyway.
const drainTimeout = stopSIGINTGrace + 2*time.Second

// tailHeadlessFile follows an NDJSON log file written by a detached or
// reattached subprocess, feeding each complete line through
// processHeadlessLine starting at startOffset. It returns exited=true when
// the run is over (process exited, or it was intentionally stopped) and
// the agent should be finalized; exited=false only when the app is
// shutting down while the process is still alive and was NOT intentionally
// stopped — the caller then leaves it running for the next reattach. The
// returned endOffset is the byte position after the last complete line
// consumed, so a retry or reattach can resume without re-reading or
// skipping lines.
func (m *Manager) tailHeadlessFile(ctx context.Context, a *Agent, path string, startOffset int64, procDone <-chan struct{}) (exited bool, endOffset int64) {
	offset := startOffset

	f, err := os.Open(path)
	if err != nil {
		m.logger.Warn("agent.headless.tail.open", "id", a.ID, "path", path, "err", err)
		select {
		case <-procDone:
			return true, offset
		case <-ctx.Done():
			return a.WasStopped(), offset
		}
	}
	defer func() { _ = f.Close() }()

	var buf []byte
	var lastEmit time.Time
	provider := normalizeProvider(a.Provider)

	// end is the byte position after the last complete line consumed: total
	// bytes read minus any trailing partial line still buffered. Resuming a
	// retry or reattach from here never re-reads or skips a line.
	end := func() int64 { return offset - int64(len(buf)) }

	// drain reads all bytes appended since offset, splits complete lines,
	// and processes them. Returns true if a guardrail asked to stop.
	drain := func() (stop bool) {
		if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
			return false
		}
		chunk, readErr := io.ReadAll(f)
		if readErr != nil || len(chunk) == 0 {
			return false
		}
		offset += int64(len(chunk))
		a.TouchLastEvent()
		buf = append(buf, chunk...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[:i]
			buf = buf[i+1:]
			if m.processHeadlessLine(ctx, a, line, &lastEmit, provider) {
				return true
			}
		}
		return false
	}

	// waitExit drains until the process exits or drainTimeout elapses, then
	// returns. Used after we have asked the process to stop.
	waitExit := func() {
		select {
		case <-procDone:
		case <-time.After(drainTimeout):
		}
		drain()
	}

	for {
		// On app shutdown, leave a detached-but-not-stopped agent running
		// for the next reattach. An intentional StopAgent (WasStopped) does
		// not survive — fall through and finalize. Checked before the select
		// so a simultaneously-ready procDone never wins the shutdown race.
		if ctx.Err() != nil && !a.WasStopped() {
			return false, end()
		}

		if drain() {
			// Guardrail kill: force the child to stop, wait for it, finalize.
			m.signalKill(a)
			waitExit()
			return true, end()
		}

		select {
		case <-procDone:
			drain()
			return true, end()
		case <-ctx.Done():
			if a.WasStopped() {
				// Intentional stop: the child is being terminated; wait for
				// it, then finalize.
				waitExit()
				return true, end()
			}
			// App shutdown: leave the detached child running for reattach.
			return false, end()
		case <-time.After(headlessTailPoll):
		}
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

func (m *Manager) streamHeadlessOutput(ctx context.Context, a *Agent, stdout io.Reader, outFile io.Writer) {
	tracked := &trackingReader{r: stdout, touch: a.TouchLastEvent}
	scanner := bufio.NewScanner(tracked)
	scanner.Buffer(make([]byte, 0, headlessScannerBuffer), headlessScannerBuffer)
	var lastEmit time.Time
	provider := normalizeProvider(a.Provider)
	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		if m.processHeadlessLine(ctx, a, line, &lastEmit, provider) {
			// Guardrail asked to stop: cancel the context so the pipe-backed
			// subprocess is terminated, then unwind. cancel may be nil in
			// unit tests that drive the streamer directly.
			if a.cancel != nil {
				a.cancel()
			}
			return
		}
	}
	m.reportScannerError(a, scanner.Err())
}

// parseHeadlessEvent parses one raw NDJSON line into a StreamEvent using the
// provider-appropriate parser. Shared by the live line handler and the
// reattach rehydrator. `provider` is the normalized provider name.
func parseHeadlessEvent(line []byte, provider string) (StreamEvent, error) {
	switch provider {
	case "codex":
		ce, err := ParseCodexLine(line)
		if err != nil {
			return StreamEvent{}, err
		}
		return codexEventToStreamEvent(ce), nil
	case "copilot":
		ce, err := ParseCopilotLine(line)
		if err != nil {
			return StreamEvent{}, err
		}
		return copilotEventToStreamEvent(ce), nil
	default:
		ce, err := ParseClaudeLine(line)
		if err != nil {
			return StreamEvent{}, err
		}
		return claudeEventToStreamEvent(ce), nil
	}
}

// processHeadlessLine parses one NDJSON line, appends and emits the event,
// captures session/plugin metadata, and applies the turn and cost
// guardrails. Returns true when a guardrail decision says to stop the
// stream. Shared by the pipe-backed streamer and the file tailer; it never
// writes the log file (the caller or the child process owns that).
func (m *Manager) processHeadlessLine(ctx context.Context, a *Agent, line []byte, lastEmit *time.Time, provider string) (stop bool) {
	event, parseErr := parseHeadlessEvent(line, provider)
	if parseErr != nil {
		m.logger.Warn("agent.headless.parse", "id", a.ID, "err", parseErr, "line", string(line))
		return false
	}
	if event.Type == "" {
		return false
	}

	event.Timestamp = time.Now().UTC()
	if event.LimitSnapshot != nil {
		snapshot := *event.LimitSnapshot
		if snapshot.Provider == "" {
			snapshot.Provider = provider
		}
		if snapshot.CapturedAt.IsZero() {
			snapshot.CapturedAt = event.Timestamp
		}
		m.mu.RLock()
		limitSink := m.limitSink
		m.mu.RUnlock()
		if limitSink != nil {
			limitSink(snapshot)
		}
	}
	a.AppendOutput(event)
	a.AddToolCalls(event.ToolCalls)
	// Feed the tool-call fingerprint into the real-time loop detector. An empty
	// signature (non-tool event) is a no-op, so the streak survives reasoning
	// between identical calls and the watchdog can spot an active loop.
	a.NoteToolSignature(event.toolSig)
	// Record any auto-mode classifier denials observed in this event's tool results.
	for _, d := range event.permissionDenials {
		a.NotePermissionDenial(d.ToolUseID, d.Reason)
	}
	if event.Type == "result" || time.Since(*lastEmit) >= headlessEmitInterval {
		m.emit(events.AgentOutput(a.ID), event)
		*lastEmit = time.Now()
	}

	// Capture the session id as soon as it appears (init/system), not only on
	// the terminal result. Claude/Codex report it at session start; without
	// this a mid-run crash leaves the registry record with an empty session
	// and restart-stale cold-restarts instead of resuming. Mirrors the
	// conversational runner (runner_convo.go) and AddResultStats.
	if (event.Type == "init" || event.Type == "system") && event.SessionID != "" {
		if a.GetSessionID() != event.SessionID {
			a.SetSessionID(event.SessionID)
			m.saveRegistry(a)
		}
		if provider == "codex" {
			if p := resolveCodexSessionFile(event.SessionID); p != "" {
				a.SetSessionFilePath(p)
			}
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
		// Copilot reports output tokens per assistant.message (claude/codex
		// carry 0 here and total on the result event instead). Accumulate them
		// as they stream so the stats reflect a copilot run that has no
		// token totals on its terminal result.
		if event.OutputTokens > 0 {
			a.AddOutputTokens(event.OutputTokens)
		}
		if keepGoing := m.checkTurnsGuardrail(ctx, a); !keepGoing {
			return true
		}
	}

	if event.Type == "result" {
		costNow := a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
		a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
		// Copilot's billing unit: premium requests (no USD on the result event).
		if event.PremiumRequests > 0 {
			a.AddPremiumRequests(event.PremiumRequests)
		}
		m.logger.Info("agent.headless.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow)
		// Persist the captured session ID so a reattach or restart-stale
		// recovery can pass --resume. No-op when survival is disabled.
		if event.SessionID != "" {
			m.saveRegistry(a)
		}
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
	return false
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
			// Caller terminates the subprocess: streamHeadlessOutput cancels
			// the context (pipe-backed); tailHeadlessFile signal-kills the
			// detached child by PID.
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
	m.fireComplete(a, false)
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
