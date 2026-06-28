package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	providerpkg "github.com/Automaat/sybra/internal/provider"
)

// makeFIFO creates (or recreates) a named pipe at path. A stale pipe from a
// prior run is removed first so the mode bits are always fresh.
func makeFIFO(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o600)
}

// startConvoProcessSurvive spawns a detached conversational Claude process
// whose stdin is a FIFO (opened O_RDWR so it never sees EOF and survives the
// parent's exit) and whose stdout is the NDJSON log file. The manager reads
// output by tailing that file; follow-up messages are written to the FIFO,
// which a reattaching instance reopens.
func (m *Manager) startConvoProcessSurvive(a *Agent, cfg RunConfig, outFile **os.File) (*exec.Cmd, error) {
	reg := m.registry()
	if reg == nil {
		return nil, fmt.Errorf("survive convo: registry not enabled")
	}

	args := m.buildConvoArgs(a, cfg)
	cmd := exec.Command("claude", args...) // no Context: a cancelled ctx must not kill a detached child
	configureDetached(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	}

	fifoPath := reg.fifoPath(a.ID)
	if err := makeFIFO(fifoPath); err != nil {
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	// O_RDWR keeps a writer on the pipe for the child's whole life, so the
	// child's read end never hits EOF even after the parent dies.
	fifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open fifo: %w", err)
	}
	cmd.Stdin = fifo
	a.convo.replaceStdinPipe(fifo)
	a.setStdinPath(fifoPath)

	// Log file is the child's stdout, so it must exist before Start. Opened
	// once and reused across retries.
	if *outFile == nil {
		f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
		if fileErr != nil {
			_ = fifo.Close()
			return nil, fmt.Errorf("open agent log: %w", fileErr)
		}
		a.SetLogPath(f.Name())
		*outFile = f
	}
	cmd.Stdout = *outFile

	stderrPath := (*outFile).Name() + ".stderr"
	stderrF, sErr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if sErr == nil {
		cmd.Stderr = stderrF
	}

	if startErr := cmd.Start(); startErr != nil {
		_ = fifo.Close()
		if stderrF != nil {
			_ = stderrF.Close()
		}
		return nil, fmt.Errorf("start claude: %w", startErr)
	}
	// The child holds its own dup of stderr; the parent copy can close.
	if stderrF != nil {
		_ = stderrF.Close()
	}

	a.SetCmd(cmd)
	a.setDetached(true)
	m.saveRegistry(a)
	return cmd, nil
}

// runConvoAttemptSurvive runs one detached conversational attempt: spawn,
// send the initial prompt, then tail the log file until the process exits or
// the app shuts down (errSurviveShutdown leaves it running for reattach).
func (m *Manager) runConvoAttemptSurvive(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64) (retry bool, err error) {
	cmd, startErr := m.startConvoProcessSurvive(a, cfg, outFile)
	if startErr != nil {
		return false, startErr
	}
	m.logger.Info("agent.convo.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir, "detached", true)

	logPath := (*outFile).Name()
	stderrPath := logPath + ".stderr"

	// Send the initial prompt only when starting a fresh session; on a resume
	// (--resume) re-sending would duplicate the turn. With no prompt and no
	// session, the agent is idle waiting for the first user message.
	if cfg.Prompt != "" && a.GetSessionID() == "" {
		if werr := m.writeUserMessage(a, cfg.Prompt); werr != nil {
			m.logger.Error("agent.convo.initial-prompt", "id", a.ID, "err", werr)
		}
	} else if cfg.Prompt == "" && a.GetSessionID() == "" {
		a.SetState(StatePaused)
		m.emit(events.AgentState(a.ID), a)
	}

	procDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(procDone)
	}()

	prevLen := len(a.ConvoOutput())
	startOffset := int64(0)
	if tailOffset != nil {
		startOffset = *tailOffset
	}
	exited, endOffset := m.tailConvoFile(ctx, a, logPath, startOffset, cfg.OneShot, procDone)
	if tailOffset != nil {
		*tailOffset = endOffset
	}
	if !exited {
		m.logger.Info("agent.convo.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown")
		return false, errSurviveShutdown
	}

	// Only inspect exit status / retry once the process has actually exited.
	// On an intentional stop the tailer can return after drainTimeout with
	// the wait goroutine still running, so reading waitErr then would race
	// the write and misread a live process as a clean exit.
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
		m.logger.Error("agent.convo.stderr", "id", a.ID, "stderr", stderrOut)
	}
	all := a.ConvoOutput()
	if prevLen > len(all) {
		prevLen = len(all)
	}
	attemptEvents := all[prevLen:]
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
		if shouldRetryConvo(stderrOut, attemptEvents, m.logger) {
			return true, nil
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	} else if m.reportCleanProviderHealthSignalConvo(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	return false, nil
}

// runConvoAttemptSurviveOneShot runs a detached one-shot conversational
// turn: the prompt is passed as a CLI argument (no stdin), output is
// streamed to the log file, and the tailer follows it until the turn
// completes and the process exits — so an interrupted one-shot step
// finishes across a restart instead of faking completion via stale
// recovery. No FIFO is created, so the registry record's StdinPath stays
// empty, which is how reattach recognizes a one-shot (tail-only).
func (m *Manager) runConvoAttemptSurviveOneShot(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64) (retry bool, err error) {
	args := m.buildOneShotConvoArgs(a, cfg)
	cmd := exec.Command("claude", args...)
	configureDetached(cmd)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), cfg.ExtraEnv...)
	}
	// No stdin: the prompt is an argument; a nil Stdin reads from /dev/null,
	// which claude -p <prompt> never touches.

	if *outFile == nil {
		f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
		if fileErr != nil {
			return false, fmt.Errorf("open agent log: %w", fileErr)
		}
		a.SetLogPath(f.Name())
		*outFile = f
	}
	cmd.Stdout = *outFile

	stderrPath := (*outFile).Name() + ".stderr"
	if stderrF, sErr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); sErr == nil {
		cmd.Stderr = stderrF
		defer func() { _ = stderrF.Close() }()
	}

	if startErr := cmd.Start(); startErr != nil {
		return false, fmt.Errorf("start claude: %w", startErr)
	}
	a.SetCmd(cmd)
	a.setDetached(true)
	// StdinPath intentionally left unset: reattach treats a record with no
	// StdinPath as a one-shot (tail-only) agent.
	m.saveRegistry(a)
	m.logger.Info("agent.convo.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir, "detached", true, "oneshot", true)

	procDone := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(procDone)
	}()

	prevLen := len(a.ConvoOutput())
	startOffset := int64(0)
	if tailOffset != nil {
		startOffset = *tailOffset
	}
	exited, endOffset := m.tailConvoFile(ctx, a, (*outFile).Name(), startOffset, true, procDone)
	if tailOffset != nil {
		*tailOffset = endOffset
	}
	if !exited {
		m.logger.Info("agent.convo.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown", "oneshot", true)
		return false, errSurviveShutdown
	}

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
		m.logger.Error("agent.convo.stderr", "id", a.ID, "stderr", stderrOut)
	}
	all := a.ConvoOutput()
	if prevLen > len(all) {
		prevLen = len(all)
	}
	attemptEvents := all[prevLen:]
	if waitErr != nil {
		m.logger.Error("agent.convo.exit", "id", a.ID, "err", waitErr)
		a.SetExitErr(waitErr)
		if shouldRetryConvo(stderrOut, attemptEvents, m.logger) {
			return true, nil
		}
		m.reportProviderHealthSignalConvo(a, stderrOut, attemptEvents)
	} else if m.reportCleanProviderHealthSignalConvo(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	return false, nil
}

// tailConvoFile follows a detached/reattached conversational agent's log
// file from startOffset, feeding each complete line through
// processConvoLine. Semantics mirror tailHeadlessFile: exited=true when the
// run is over (process exited, or intentional stop), false only on app
// shutdown of a still-running, not-stopped agent (leave it for reattach).
func (m *Manager) tailConvoFile(ctx context.Context, a *Agent, path string, startOffset int64, oneShot bool, procDone <-chan struct{}) (exited bool, endOffset int64) {
	offset := startOffset

	f, err := os.Open(path)
	if err != nil {
		m.logger.Warn("agent.convo.tail.open", "id", a.ID, "path", path, "err", err)
		select {
		case <-procDone:
			return true, offset
		case <-ctx.Done():
			return a.WasStopped(), offset
		}
	}
	defer func() { _ = f.Close() }()

	var buf []byte
	st := &convoEmitState{}
	end := func() int64 { return offset - int64(len(buf)) }

	drain := func() {
		if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
			return
		}
		chunk, readErr := io.ReadAll(f)
		if readErr != nil || len(chunk) == 0 {
			return
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
			m.processConvoLine(a, line, st, oneShot)
		}
	}

	for {
		if ctx.Err() != nil && !a.WasStopped() {
			return false, end()
		}
		drain()
		select {
		case <-procDone:
			drain()
			m.flushConvo(a, st)
			return true, end()
		case <-ctx.Done():
			if a.WasStopped() {
				select {
				case <-procDone:
				case <-time.After(drainTimeout):
				}
				drain()
				m.flushConvo(a, st)
				return true, end()
			}
			return false, end()
		case <-time.After(headlessTailPoll):
		}
	}
}

// rehydrateConvoFromLog replays a conversational agent's log into its convo
// buffer (and session id) without emitting events, returning the byte offset
// just past the last complete line for the tailer to resume from.
func rehydrateConvoFromLog(a *Agent, path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return 0
	}

	var offset int64
	start := 0
	for i := range data {
		if data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		offset = int64(start)
		parsed, perr := ParseClaudeLine(line)
		if perr != nil {
			continue
		}
		ev := claudeEventToConvoEvent(parsed)
		if ev.Type == "" {
			continue
		}
		a.AppendConvo(ev)
		if ev.Type == "system" && ev.SessionID != "" {
			a.SetSessionID(ev.SessionID)
		}
		if ev.Type == "result" {
			a.AddResultStats(ev.SessionID, ev.CostUSD, ev.InputTokens, ev.OutputTokens, ev.ReasoningTokens)
			a.AddCacheStats(ev.CacheCreationInputTokens, ev.CacheReadInputTokens)
		}
	}
	return offset
}

// reattachConvo resumes a live detached conversational agent: reopen its
// stdin FIFO so follow-ups still reach the child, then tail its log until it
// exits or the app shuts down.
func (m *Manager) reattachConvo(ctx context.Context, a *Agent, startOffset int64, procStart string, oneShot bool) {
	// A one-shot agent's stdin (a regular file) was already consumed; only an
	// interactive session needs its FIFO reopened for follow-ups.
	if !oneShot {
		if sp := a.GetStdinPath(); sp != "" {
			if fifo, err := os.OpenFile(sp, os.O_RDWR, 0); err == nil {
				a.convo.setStdinPipe(fifo)
			} else {
				m.logger.Warn("agent.reattach.fifo", "id", a.ID, "path", sp, "err", err)
			}
		}
	}

	procDone := make(chan struct{})
	go watchPID(ctx, a.GetPID(), procStart, procDone)

	exited, _ := m.tailConvoFile(ctx, a, a.GetLogPath(), startOffset, oneShot, procDone)
	if !exited {
		m.logger.Info("agent.reattach.detach", "id", a.ID, "pid", a.GetPID(), "reason", "shutdown")
		return
	}

	// No cmd.Wait runs for a reattached agent, so GetExitErr is nil. Infer
	// the outcome from the terminal result: none -> crash (errReattachedGone);
	// an error result -> a real failure; a success result -> leave nil.
	if found, isError := a.lastConvoResult(); !found {
		a.SetExitErr(errReattachedGone)
	} else if isError {
		a.SetExitErr(errReattachedResultError)
		m.reportProviderHealthSignalConvo(a, "", a.ConvoOutput())
	} else if m.reportCleanProviderHealthSignalConvo(a, "", a.ConvoOutput()) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
	}
	a.SetState(StateStopped)
	m.logger.Info("agent.reattach.convo.done", "id", a.ID, "cost", a.GetCostUSD())
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(a, a.GetExitErr() == nil)
	m.markAgentDone(a)
}
