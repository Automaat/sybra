package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Automaat/sybra/internal/errclass"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/logging"
	"github.com/Automaat/sybra/internal/project"
	providerpkg "github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/textutil"
)

// errSurviveShutdown is returned by a detached headless attempt when the
// app's context is cancelled (shutdown) while the subprocess is still
// alive. The runner returns without finalizing so the agent is left
// running for the next instance to reattach.
var errSurviveShutdown = errors.New("agent: detached, leaving process running for reattach")

// errStoppedPendingReap is recorded as the agent's exit error when an
// intentional stop (guardrail kill or StopAgent) signalled the subprocess but
// it had not been reaped by cmd.Wait() within drainTimeout. Without this,
// finalizeRun/OnComplete would see a nil ExitErr — the zero value carried
// over from before the kill — and misreport the killed partial run as a
// clean success in cost/audit metrics.
var errStoppedPendingReap = errors.New("agent: stopped, subprocess not yet reaped")
var errCostGuardrailExceeded = errors.New("agent: cost guardrail exceeded")
var errCheckpointCommitFailed = errors.New("agent: checkpoint commit failed")

// errBackgroundTaskLiveAtExit is recorded as the agent's exit error when the
// headless subprocess exits (naturally, or after the post-result-hang guard
// gave up waiting) while the CLI's last known state still shows a live
// `run_in_background` bash task. The process exiting kills that task
// mid-write, which can silently corrupt the worktree (task 3aeabb65); a run
// left this way must not be reported as a clean success (ExitErr nil) to
// OnComplete/fireComplete, since the workflow would otherwise hand a
// possibly-corrupted worktree to the next stage.
var errBackgroundTaskLiveAtExit = errors.New("agent: process exited while a background bash task was still running")

// checkLiveBackgroundTasksAtExit returns errBackgroundTaskLiveAtExit if the
// agent's last reported CLI state still shows a live background bash task.
// Called at every point a one-shot attempt (headless, or interactive OneShot)
// is about to report a clean exit, so the guardrail described on
// backgroundTaskGuardrail is enforced even when the provider ignores the
// prompt instruction and ends its turn with a task still running.
//
// An intentional StopAgent is exempt: stopping a session (e.g. closing stdin
// on a regular interactive run) produces a clean process exit, but that is a
// deliberate teardown, not a runaway process leaving the worktree corrupt, so
// it must not be mislabeled as a failed run.
func checkLiveBackgroundTasksAtExit(m *Manager, a *Agent) error {
	if a.WasStopped() {
		return nil
	}
	if !a.HasBackgroundTasks() {
		return nil
	}
	m.logger.Warn("agent.headless.exit_with_live_background_tasks", "id", a.ID,
		"hint", "headless process exited while a background bash task was still running; the run is marked failed instead of a clean success to prevent handing off a possibly-corrupted worktree")
	return errBackgroundTaskLiveAtExit
}

// headlessTailPoll is how often the detached/reattached tailer polls the
// log file for new NDJSON lines.
const headlessTailPoll = 100 * time.Millisecond

// postResultFastCloseDelay is the short debounce before a pipe-backed
// headless run cancels a process that already emitted a clean terminal
// result with no live cleanup work. Any further output within this window
// proves the earlier result was not the final close boundary and suppresses
// the cancel.
var postResultFastCloseDelay = 100 * time.Millisecond

// postResultGrace bounds how long the tailer waits for a headless process to
// exit on its own after it has emitted a terminal (non-error) result event.
// CC normally exits within a second or two; a skill that spawns subagents or
// leaves a background task alive can hang the process indefinitely after the
// final result (observed in task c4a0fda0, where a staff-code-review workflow
// finished but the process never exited and the stall watchdog mis-escalated
// the completed run to human-required). When the stream ends in a result and
// stays idle past this window the run is logically complete, so the tailer
// stops the orphaned process and finalizes from the result. Any further
// output re-arms the clock, so a provider that legitimately emits multiple
// result events is never cut off mid-work.
const postResultGrace = 90 * time.Second

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

// MaxPendingHeadlessSteerPrompts bounds queued operator guidance while a
// headless turn is still running. The queue is replayed into the survival
// registry, so keep it finite rather than allowing a stuck turn to grow memory
// and restart-replay state without limit. Exported so e2e tests outside this
// package can assert against the real cap instead of a duplicated literal.
const MaxPendingHeadlessSteerPrompts = 20

type preparedHeadlessAttempt struct {
	cfg     RunConfig
	inv     headlessInvocation
	cleanup func()
}

func (m *Manager) ensureHeadlessOutputFile(a *Agent, outFile **os.File) {
	if outFile == nil || *outFile != nil {
		return
	}
	f, fileErr := logging.NewAgentOutputFile(m.logDir, a.ID)
	if fileErr != nil {
		m.logger.Error("agent.output.file", "id", a.ID, "err", fileErr)
	}
	if f != nil {
		a.SetLogPath(f.Name())
		*outFile = f
	}
}

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

	if earlyReturn := m.runRetryLoop(ctx, a, "headless", func(int) (bool, error) {
		return m.runHeadlessAttempt(ctx, a, cfg, &outFile, &tailOffset)
	}); earlyReturn {
		return
	}

	// If CC exited after a graceful SIGINT (WasStopped + session_id captured
	// from the final result event), the next run can pass --resume.
	if a.WasStopped() && a.GetSessionID() != "" {
		a.SetResumable(true)
	}
	// finalizeRun releases the agent only after onComplete returns.
	// HasRunningAgentForTask gates ResumeStalled; releasing it before the
	// workflow advance handler runs lets a tight ResumeStalled loop dispatch
	// a duplicate agent.
	m.finalizeRun(ctx, a, "agent.headless.done")
}

func (m *Manager) runHeadlessAttempt(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, tailOffset *int64) (retry bool, err error) {
	if outFile == nil {
		return false, nil
	}
	a.ClearPostResultWait()

	prepared, err := prepareHeadlessAttempt(a, cfg)
	if err != nil {
		return false, err
	}
	defer prepared.cleanup()
	a.SetForkSubagent(prepared.cfg.ForkSubagent && prepared.inv.name == providerid.Claude)

	if _, _, unrendered := a.GetPromptRender(); len(unrendered) > 0 {
		m.logger.Warn("agent.headless.skill_unrendered", "id", a.ID, "provider", prepared.inv.name, "skills", unrendered)
	}

	if m.survives() && a.Mode == "headless" {
		inv := prepared.inv
		return m.runHeadlessAttemptSurvive(ctx, a, prepared.cfg, outFile, tailOffset, inv.name, inv.args, inv.env, inv.command)
	}
	return m.runHeadlessAttemptPipe(ctx, a, prepared.cfg, outFile, prepared.inv)
}

func prepareHeadlessAttempt(a *Agent, cfg RunConfig) (preparedHeadlessAttempt, error) {
	prepared := preparedHeadlessAttempt{cfg: cfg, cleanup: func() {}}
	prov, err := providerForInvocation(a, cfg)
	if err != nil {
		return prepared, err
	}
	if prov.OutputSchemaAsFile() && cfg.OutputSchema != "" {
		f, schemaErr := os.CreateTemp("", "sybra-codex-schema-*.json")
		if schemaErr != nil {
			return prepared, fmt.Errorf("create codex output schema: %w", schemaErr)
		}
		if _, wErr := f.WriteString(cfg.OutputSchema); wErr != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return prepared, fmt.Errorf("write codex output schema: %w", wErr)
		}
		_ = f.Close()
		cfg.outputSchemaPath = f.Name()
		prepared.cfg = cfg
		prepared.cleanup = func() { _ = os.Remove(cfg.outputSchemaPath) }
	}

	name, args, invokeEnv, command, err := buildHeadlessInvocation(a, cfg)
	if err != nil {
		prepared.cleanup()
		prepared.cleanup = func() {}
		return prepared, err
	}
	prepared.inv = headlessInvocation{name: name, args: args, env: invokeEnv, command: command}
	return prepared, nil
}

// steerableHeadlessInvocation reports whether this attempt was built with the
// stdin/stream-json shape: cfg.HeadlessSteerable requests it, but only
// claudeProvider.BuildHeadlessInvocation actually honors the field — codex
// and copilot ignore it and keep their normal one-shot argument shape, so a
// stdin pipe/FIFO must never be attached for them (nothing would ever read
// or need it).
func steerableHeadlessInvocation(cfg RunConfig, providerName string) bool {
	return cfg.HeadlessSteerable && providerName == providerid.Claude
}

// stdinPromptHeadlessInvocation reports whether this attempt needs the
// caller to write its prompt to stdin as a plain one-shot blob (see
// claudeProvider.BuildHeadlessInvocation's non-steerable branch) — true only
// for a non-steerable claude run. A steerable claude run also delivers its
// prompt over stdin, but through the pre-existing writeUserMessage/convo
// pipe path, so it's excluded here; codex/copilot still receive the prompt
// positionally and are excluded too.
func stdinPromptHeadlessInvocation(steerable bool, providerName string) bool {
	return !steerable && providerName == providerid.Claude
}

// writeAndCloseHeadlessPrompt sends the one-shot stdin prompt then closes the
// pipe so the child (default --input-format text) sees EOF immediately,
// matching a plain `claude -p <text>` run's stdin behavior. Never returns an
// error to the caller: a write failure here should not abort an otherwise
// healthy attempt, only be logged for diagnosis.
func (m *Manager) writeAndCloseHeadlessPrompt(a *Agent, stdin io.WriteCloser, prompt string) {
	if _, err := io.WriteString(stdin, prompt); err != nil {
		m.logger.Error("agent.headless.initial-prompt", "id", a.ID, "err", err)
	}
	_ = stdin.Close()
}

func (m *Manager) runHeadlessAttemptPipe(ctx context.Context, a *Agent, cfg RunConfig, outFile **os.File, inv headlessInvocation) (retry bool, err error) {
	cmd := newProviderCmd(ctx, &cfg, false, inv.name, inv.args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 || len(inv.env) > 0 {
		cmd.Env = append(os.Environ(), inv.env...)
		cmd.Env = append(cmd.Env, cfg.ExtraEnv...)
	}
	a.SetCommand(inv.command)

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		return false, fmt.Errorf("stdout pipe: %w", pipeErr)
	}

	steerable := steerableHeadlessInvocation(cfg, inv.name)
	stdinPrompt := stdinPromptHeadlessInvocation(steerable, inv.name) && cfg.Prompt != ""
	var promptStdin io.WriteCloser
	if steerable {
		stdinPipe, stdinErr := cmd.StdinPipe()
		if stdinErr != nil {
			return false, fmt.Errorf("stdin pipe: %w", stdinErr)
		}
		a.convo.replaceStdinPipe(stdinPipe)
		a.setFinalizing(false)
		// setFinalizing refreshed CanSteer; emit so the UI shows steer controls
		// as soon as the stdin transport is live, not only on a later event.
		m.emit(events.AgentState(a.ID), a)
	} else if stdinPrompt {
		stdinPipe, stdinErr := cmd.StdinPipe()
		if stdinErr != nil {
			return false, fmt.Errorf("stdin pipe: %w", stdinErr)
		}
		promptStdin = stdinPipe
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		if steerable {
			a.convo.closeStdinPipe()
		} else if promptStdin != nil {
			_ = promptStdin.Close()
		}
		return false, fmt.Errorf("start %s: %w", inv.name, startErr)
	}
	a.SetCmd(cmd)

	// Steerable: the prompt is the first user message over stdin, mirroring
	// the conversational runner's initial-prompt handling — a resumed run
	// (session already set) must not resend it, since the session already
	// has it. Non-steerable stdin-prompt: always resent on every attempt,
	// matching what the old positional-argv delivery did unconditionally.
	if steerable && cfg.Prompt != "" && a.GetSessionID() == "" {
		// Unlike a steer message, the initial prompt is the run's only chance to
		// reach the child — on failure the child gets EOF having never seen the
		// task, so a log-and-continue would silently strand the run. Fail the
		// attempt instead of pressing on as if delivery succeeded.
		if writeErr := m.writeUserMessageTimeout(a, cfg.Prompt, stdinInitialWriteTimeout); writeErr != nil {
			m.logger.Error("agent.headless.initial-prompt", "id", a.ID, "err", writeErr)
			a.SetError(ErrorKindPromptUndelivered, writeErr.Error())
			a.SetExitErr(fmt.Errorf("%w: %w", errPromptUndelivered, writeErr))
			a.convo.closeStdinPipe()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
			return false, fmt.Errorf("deliver initial prompt: %w: %w", errPromptUndelivered, writeErr)
		}
	} else if promptStdin != nil {
		m.writeAndCloseHeadlessPrompt(a, promptStdin, cfg.Prompt)
	}

	// Open log file on first successful start; subsequent retries append to same file.
	m.ensureHeadlessOutputFile(a, outFile)

	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir)

	var logWriter io.Writer
	if outFile != nil && *outFile != nil {
		logWriter = *outFile
	}

	prevLen := len(a.Output())
	m.streamHeadlessOutput(ctx, a, stdout, logWriter)

	if steerable {
		// The child may still be waiting on a never-closed stdin (no steer
		// message queued at the last result, but the finalizing close raced
		// with the child's own exit) — make sure it is not left open past
		// this attempt.
		a.convo.closeStdinPipe()
	}

	waitErr := cmd.Wait()

	// Post-result-hang finalize: the watchdog's checkCompletedHang can kill a
	// non-detached process after it already emitted a terminal result (this
	// path has no live tailer to catch that itself), so the signal error in
	// waitErr is expected and not a failure. Completion comes from the result
	// event, mirroring the detached path's own handling in
	// runHeadlessAttemptSurvive.
	if a.wasCompletedByResult() {
		m.finalizeFromResult(a, prevLen)
		return false, nil
	}

	stderrOut := stderrBuf.String()
	if a.WasStopped() && a.GetEscalationReason() == EscalationReasonCost {
		a.SetExitErr(errCostGuardrailExceeded)
		logAttemptStderr(m.logger, "agent.headless.stderr", a.ID, stderrOut, a.GetExitErr())
		return false, nil
	}
	if retry := m.resolveHeadlessAttemptExit(a, waitErr, stderrOut, prevLen); retry {
		return true, nil
	}
	return false, nil
}

// makeFIFO creates (or recreates) a named pipe at path. A stale pipe from a
// prior run is removed first so the mode bits are always fresh.
func makeFIFO(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o600)
}

// startHeadlessProcessSurviveStdin assigns a steerable detached headless
// claude process's stdin to a FIFO (reusing makeFIFO/agentFIFOPath). Unlike a
// conversational agent — which is handed an O_RDWR fd so it keeps a writer
// across an app restart — a steerable headless run needs the same property.
// Giving it a read-only fd makes the parent's writer the last writer, so a
// deploy closes stdin and silently ends the detached child before reattach.
// The normal terminal-result path already has an explicit finalizing state and
// post-result kill, so EOF is neither needed nor safe as its close signal.
//
// The returned *os.File is the parent's copy of the child's O_RDWR end; the
// caller closes it after cmd.Start(), leaving the child as the restart-safe
// FIFO anchor.
// Called from startHeadlessSurviveProcess before cmd.Start(); on any error
// cmd.Stdin is left unset and the caller aborts the attempt.
func (m *Manager) startHeadlessProcessSurviveStdin(a *Agent, cmd *exec.Cmd) (*os.File, error) {
	fifoPath := agentFIFOPath(m.registryDir(), a.ID)
	if err := makeFIFO(fifoPath); err != nil {
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	// O_RDWR never blocks on a FIFO. Both parent and child retain a writer, so
	// a process that survives an app restart cannot observe a false EOF.
	writeFifo, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open fifo (write): %w", err)
	}
	childStdin, err := os.OpenFile(fifoPath, os.O_RDWR, 0)
	if err != nil {
		_ = writeFifo.Close()
		return nil, fmt.Errorf("open fifo (child): %w", err)
	}
	cmd.Stdin = childStdin
	a.convo.replaceStdinPipe(writeFifo)
	a.setStdinPath(fifoPath)
	a.setFinalizing(false)
	return childStdin, nil
}

// startHeadlessSurviveProcess builds and starts the detached subprocess for a
// survive-mode headless attempt: wires stdout to the shared log file, wires
// stderr to a sibling ".stderr" file, attaches a steerable claude run's FIFO
// stdin (see startHeadlessProcessSurviveStdin), and delivers the initial
// prompt over that FIFO for a fresh (non-resumed) session. Split out of
// runHeadlessAttemptSurvive to keep it under the package's function-length
// lint budget.
func (m *Manager) startHeadlessSurviveProcess(ctx context.Context, a *Agent, cfg RunConfig, outFile *os.File, name string, args, invokeEnv []string, command string) (*exec.Cmd, error) {
	cmd := newProviderCmd(ctx, &cfg, true, name, args...)
	if a.sessionCWD != "" {
		cmd.Dir = a.sessionCWD
	}
	if len(cfg.ExtraEnv) > 0 || len(invokeEnv) > 0 {
		cmd.Env = append(os.Environ(), invokeEnv...)
		cmd.Env = append(cmd.Env, cfg.ExtraEnv...)
	}
	a.SetCommand(command)
	cmd.Stdout = outFile

	// Steerable claude runs get an O_RDWR FIFO stdin, exactly like a detached
	// conversational agent. The child retains a writer across process handoff,
	// so a server restart cannot deliver a false EOF before reattach.
	// Only claude honors HeadlessSteerable (see steerableHeadlessInvocation);
	// codex/copilot keep the plain no-stdin invocation unchanged.
	steerable := steerableHeadlessInvocation(cfg, name)
	stdinPrompt := stdinPromptHeadlessInvocation(steerable, name) && cfg.Prompt != ""
	var childStdin *os.File
	var promptStdin io.WriteCloser
	if steerable {
		cs, err := m.startHeadlessProcessSurviveStdin(a, cmd)
		if err != nil {
			return nil, err
		}
		childStdin = cs
	} else if stdinPrompt {
		stdinPipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin pipe: %w", err)
		}
		promptStdin = stdinPipe
	}

	stderrPath := outFile.Name() + ".stderr"
	if stderrF, ferr := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); ferr == nil {
		cmd.Stderr = stderrF
		defer func() { _ = stderrF.Close() }()
	}

	if startErr := cmd.Start(); startErr != nil {
		if steerable {
			if childStdin != nil {
				_ = childStdin.Close()
			}
			a.convo.closeStdinPipe()
		} else if promptStdin != nil {
			_ = promptStdin.Close()
		}
		return nil, fmt.Errorf("start %s: %w", name, startErr)
	}
	// The child holds its own O_RDWR dup; drop the parent's copy after start.
	if childStdin != nil {
		_ = childStdin.Close()
	}
	a.SetCmd(cmd)
	a.setDetached(true)
	m.saveRegistry(ctx, a)
	if steerable {
		// setFinalizing(false) in startHeadlessProcessSurviveStdin refreshed
		// CanSteer; emit so the UI shows steer controls as soon as the FIFO
		// transport is live, not only on a later event.
		m.emit(events.AgentState(a.ID), a)
	}
	m.logger.Info("agent.headless.start", "id", a.ID, "pid", cmd.Process.Pid, "dir", cmd.Dir, "detached", true)

	// Steerable: the prompt is the first user message over the FIFO,
	// mirroring runConvoAttemptSurvive — a resumed run (session already set)
	// must not resend it. Non-steerable stdin-prompt: always resent on every
	// attempt, matching what the old positional-argv delivery did
	// unconditionally.
	if steerable && cfg.Prompt != "" && a.GetSessionID() == "" {
		// Unlike a steer message, the initial prompt is the run's only chance to
		// reach the child — on failure the child gets EOF having never seen the
		// task, so a log-and-continue would silently strand the detached run.
		// Fail the attempt instead of pressing on as if delivery succeeded.
		if writeErr := m.writeUserMessageTimeout(a, cfg.Prompt, stdinInitialWriteTimeout); writeErr != nil {
			m.logger.Error("agent.headless.initial-prompt", "id", a.ID, "err", writeErr)
			a.SetError(ErrorKindPromptUndelivered, writeErr.Error())
			a.SetExitErr(fmt.Errorf("%w: %w", errPromptUndelivered, writeErr))
			a.convo.closeStdinPipe()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				// This process is the killed child's real OS parent, and the
				// caller only starts its cmd.Wait() reaper after a successful
				// start — so reap here to avoid leaking a zombie, mirroring the
				// pipe variant's kill path.
				_ = cmd.Wait()
			}
			return nil, fmt.Errorf("deliver initial prompt: %w: %w", errPromptUndelivered, writeErr)
		}
	} else if promptStdin != nil {
		m.writeAndCloseHeadlessPrompt(a, promptStdin, cfg.Prompt)
	}
	return cmd, nil
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
	stderrPath := logPath + ".stderr"

	cmd, startErr := m.startHeadlessSurviveProcess(ctx, a, cfg, *outFile, name, args, invokeEnv, command)
	if startErr != nil {
		return false, startErr
	}

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
		if a.WasStopped() {
			a.SetExitErr(errStoppedPendingReap)
		}
		return false, nil
	}

	// Post-result-hang finalize: the tailer stopped a process that had already
	// emitted a terminal result, so the kill signal in waitErr is expected and
	// not a failure. Completion status comes from the result event.
	if a.wasCompletedByResult() {
		m.finalizeFromResult(a, prevLen)
		return false, nil
	}

	var stderrOut string
	if b, readErr := os.ReadFile(stderrPath); readErr == nil {
		stderrOut = string(b)
	}
	if a.WasStopped() && a.GetEscalationReason() == EscalationReasonCost {
		a.SetExitErr(errCostGuardrailExceeded)
		logAttemptStderr(m.logger, "agent.headless.stderr", a.ID, stderrOut, a.GetExitErr())
		return false, nil
	}
	if retry := m.resolveHeadlessAttemptExit(a, waitErr, stderrOut, prevLen); retry {
		return true, nil
	}
	return false, nil
}

// resolveHeadlessAttemptExit inspects a headless attempt's process wait
// error and stream events once the process has actually exited, sets the
// agent's final ExitErr, and reports whether the caller should retry.
// Shared by runHeadlessAttemptPipe and runHeadlessAttemptSurvive, which
// otherwise duplicate this classification verbatim.
func (m *Manager) resolveHeadlessAttemptExit(a *Agent, waitErr error, stderrOut string, prevLen int) (retry bool) {
	if streamErr := resultStreamError(attemptEventsFrom(a.Output(), prevLen)); waitErr == nil && streamErr != nil {
		waitErr = streamErr
	}
	// Only inspect the events produced during this attempt. Some CLIs report
	// quota exhaustion as an exit-0 result event, so classify provider health
	// even when the process itself looked successful.
	attemptEvents := attemptEventsFrom(a.Output(), prevLen)
	if toolUseAborted(attemptEvents) {
		a.SetError(ErrorKindToolUseAborted, "provider run aborted after tool use was rejected")
	} else if userInterruptedStream(attemptEvents) {
		a.SetError(ErrorKindUserInterrupted, "provider run was interrupted by the user before completion")
	}
	switch {
	case waitErr != nil:
		a.SetExitErr(waitErr)
		m.logger.Error("agent.headless.exit", "id", a.ID, "err", waitErr)
		if shouldRetry(stderrOut, attemptEvents, m.logger) {
			logAttemptStderr(m.logger, "agent.headless.stderr", a.ID, stderrOut, a.GetExitErr())
			return true
		}
		m.reportProviderHealthSignal(a, stderrOut, attemptEvents)
	case m.reportCleanProviderHealthSignal(a, stderrOut, attemptEvents) == providerpkg.SignalRateLimit:
		a.SetExitErr(errProviderRateLimited)
	default:
		a.SetExitErr(checkLiveBackgroundTasksAtExit(m, a))
	}
	logAttemptStderr(m.logger, "agent.headless.stderr", a.ID, stderrOut, a.GetExitErr())
	return false
}

// finalizeFromResult sets the exit status of a run that the post-result-hang
// guard stopped, deriving completion from the terminal result event rather
// than the kill signal that appears in the process wait error.
func (m *Manager) finalizeFromResult(a *Agent, prevLen int) {
	if a.GetEscalationReason() == EscalationReasonCheckpointFailed {
		a.SetExitErr(errCheckpointCommitFailed)
		return
	}
	evs := attemptEventsFrom(a.Output(), prevLen)
	if toolUseAborted(evs) {
		a.SetError(ErrorKindToolUseAborted, "provider run aborted after tool use was rejected")
	} else if userInterruptedStream(evs) {
		a.SetError(ErrorKindUserInterrupted, "provider run was interrupted by the user before completion")
	}
	if streamErr := resultStreamError(evs); streamErr != nil {
		a.SetExitErr(streamErr)
		m.reportProviderHealthSignal(a, "", evs)
		return
	}
	if m.reportCleanProviderHealthSignal(a, "", evs) == providerpkg.SignalRateLimit {
		a.SetExitErr(errProviderRateLimited)
		return
	}
	a.SetExitErr(checkLiveBackgroundTasksAtExit(m, a))
}

func toolUseAborted(streamEvents []StreamEvent) bool {
	for i := range slices.Backward(streamEvents) {
		e := streamEvents[i]
		if e.Type != "result" {
			continue
		}
		content := strings.ToLower(e.Content)
		return e.TerminalReason == "aborted_tools" ||
			strings.Contains(content, "request interrupted by user for tool use") ||
			strings.Contains(content, "tool use was rejected")
	}
	return false
}

func userInterruptedStream(streamEvents []StreamEvent) bool {
	for i := range slices.Backward(streamEvents) {
		e := streamEvents[i]
		if e.Type != "result" {
			continue
		}
		content := strings.ToLower(e.Content)
		return e.TerminalReason == "aborted_streaming" ||
			strings.Contains(content, "request interrupted by user")
	}
	return false
}

// logAttemptStderr logs a completed attempt's captured stderr. Codex (and
// other CLIs) routinely write informational lines to stderr on healthy runs
// (e.g. "Reading additional input from stdin..."), so logging unconditionally
// at ERROR made every run look like a crash and actively misled diagnosis of
// task 133a5d46 / issue #1560. Only a genuine failure warrants ERROR; a clean
// exit logs the same content at DEBUG for anyone still chasing provider noise.
func logAttemptStderr(logger *slog.Logger, eventName, id, stderrOut string, finalErr error, attrs ...any) {
	if stderrOut == "" {
		return
	}
	kv := make([]any, 0, len(attrs)+4)
	kv = append(kv, "id", id)
	kv = append(kv, attrs...)
	kv = append(kv, "stderr", stderrOut)
	if finalErr != nil {
		logger.Error(eventName, kv...)
		return
	}
	logger.Debug(eventName, kv...)
}

func resultStreamError(streamEvents []StreamEvent) error {
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

// drainTimeout bounds how long the tailer waits for a process it just
// asked to stop (guardrail kill or StopAgent) to actually exit before it
// gives up and finalizes anyway. Var (not const) so tests can shrink it
// instead of waiting out the real grace window.
var drainTimeout = stopSIGINTGrace + 2*time.Second

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
	prov, providerErr := lookupProvider(a.Provider)
	if providerErr != nil {
		m.logger.Error("agent.headless.tail.provider", "id", a.ID, "provider", a.Provider, "err", providerErr)
		a.SetError("provider", providerErr.Error())
		return true, offset
	}

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
			if m.processHeadlessLine(ctx, a, line, &lastEmit, prov) {
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
			// A turns escalation may be waiting when shutdown cancels ctx. The
			// guardrail reports stop in that case, but the detached child was not
			// intentionally stopped and must survive for reattach.
			if ctx.Err() != nil && !a.WasStopped() {
				return false, end()
			}
			// Guardrail kill: force the child to stop, wait for it, finalize.
			m.signalKill(a)
			waitExit()
			return true, end()
		}

		if m.stopTailPostResultWait(a, waitExit) {
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
	prov, providerErr := lookupProvider(a.Provider)
	if providerErr != nil {
		m.logger.Error("agent.headless.stream.provider", "id", a.ID, "provider", a.Provider, "err", providerErr)
		a.SetError("provider", providerErr.Error())
		return
	}
	for scanner.Scan() {
		line := scanner.Bytes()

		if outFile != nil {
			_, _ = outFile.Write(line)
			_, _ = outFile.Write([]byte("\n"))
		}

		if m.processHeadlessLine(ctx, a, line, &lastEmit, prov) {
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
// reattach rehydrator.
func parseHeadlessEvent(line []byte, provider Provider) (StreamEvent, error) {
	return provider.ParseHeadlessLine(line)
}

// processHeadlessLine parses one NDJSON line, appends and emits the event,
// captures session/plugin metadata, and applies the turn and cost
// guardrails. Returns true when a guardrail decision says to stop the
// stream. Shared by the pipe-backed streamer and the file tailer; it never
// writes the log file (the caller or the child process owns that).
func (m *Manager) processHeadlessLine(ctx context.Context, a *Agent, line []byte, lastEmit *time.Time, provider Provider) (stop bool) {
	event, parseErr := parseHeadlessEvent(line, provider)
	if parseErr != nil {
		m.logger.Warn("agent.headless.parse", "id", a.ID, "err", parseErr, "line", string(line))
		return false
	}
	if event.Type == "" {
		return false
	}
	if isToolFailureDiagnosticEvent(event.Type) {
		return false
	}
	event = bindToolResultEvent(a.TaskID, string(a.EffectiveRole()), m.artifacts, event)

	event.Timestamp = time.Now().UTC()
	if event.LimitSnapshot != nil {
		snapshot := *event.LimitSnapshot
		if snapshot.Provider == "" {
			snapshot.Provider = provider.Name()
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
	if event.Type == "usage" && event.Content == "" && event.LimitSnapshot != nil {
		return false
	}
	a.applyStreamEventState(event)
	a.AppendOutput(event)
	a.AddToolCalls(event.ToolCalls)
	a.NoteSubagentCall(event.parentToolUseID)
	// Feed the tool-call fingerprint into the real-time loop detector. An empty
	// signature (non-tool event) is a no-op, so the low-progress window survives
	// reasoning between repeated actions and the watchdog can spot an active loop.
	a.NoteToolAction(event.toolSig, event.toolLoopLabel)
	// Record any auto-mode classifier denials observed in this event's tool results.
	for _, d := range event.permissionDenials {
		a.NotePermissionDenial(d.ToolUseID, d.Reason)
	}
	m.recordToolResultFailures(a, event)
	m.recordTerminalFailure(a, event)
	if event.Type == "result" || time.Since(*lastEmit) >= headlessEmitInterval {
		m.emit(events.AgentOutput(a.ID), event)
		*lastEmit = time.Now()
	}
	m.handleMalformedToolResults(a, event)

	// Capture the session id as soon as it appears (init/system), not only on
	// the terminal result. Claude/Codex report it at session start; without
	// this a mid-run crash leaves the registry record with an empty session
	// and restart-stale cold-restarts instead of resuming. Mirrors AddResultStats.
	if (event.Type == "init" || event.Type == "system") && event.SessionID != "" {
		if a.GetSessionID() != event.SessionID {
			a.SetSessionID(event.SessionID)
			m.saveRegistry(ctx, a)
		}
		if p := provider.SessionFilePath(event.SessionID); p != "" {
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
		if stop := m.applyAssistantGuardrails(ctx, a, event); stop {
			return true
		}
	}

	if event.Type == "result" {
		if keepGoing := m.handleHeadlessResult(ctx, a, event); !keepGoing {
			return true
		}
	} else {
		if reason, _, ok := a.PostResultWait(); ok {
			if reason == postResultWaitBackgroundTask && !a.HasBackgroundTasks() && a.UsesForkSubagent() {
				a.SetPostResultWait(postResultWaitForkSubagent, a.GetLastEventAt())
				reason = postResultWaitForkSubagent
			}
			m.maybeArmPipePostResultClose(a, a.GetLastEventAt(), postResultPipeResolution(reason))
		}
	}
	return false
}

func postResultPipeResolution(reason string) string {
	switch reason {
	case postResultWaitBackgroundTask:
		return "background_tasks_cleared"
	case postResultWaitForkSubagent:
		return "fork_subagent_idle"
	default:
		return "pipe_fast_close"
	}
}

func shouldTrackPostResultWait(a *Agent) bool {
	return a.PendingPromptCount() == 0 && (!a.convo.hasStdinPipe() || a.isFinalizing())
}

func ensurePostResultWaitState(a *Agent) {
	if _, _, ok := a.PostResultWait(); ok || !a.CompletedSuccessfully() || !shouldTrackPostResultWait(a) {
		return
	}
	reason := postResultWaitFastClose
	switch {
	case a.HasBackgroundTasks():
		reason = postResultWaitBackgroundTask
	case a.UsesForkSubagent():
		reason = postResultWaitForkSubagent
	}
	a.SetPostResultWait(reason, a.GetLastEventAt())
}

func (m *Manager) stopTailPostResultWait(a *Agent, waitExit func()) bool {
	ensurePostResultWaitState(a)
	reason, since, ok := a.PostResultWait()
	if !ok {
		return false
	}
	if reason == postResultWaitBackgroundTask && !a.HasBackgroundTasks() && a.UsesForkSubagent() {
		a.SetPostResultWait(postResultWaitForkSubagent, a.GetLastEventAt())
		reason = postResultWaitForkSubagent
	}
	switch {
	case reason == postResultWaitFastClose:
		m.logPostResultWaitDone(a, "reattach_fast_close")
	case reason == postResultWaitBackgroundTask && !a.HasBackgroundTasks():
		m.logPostResultWaitDone(a, "background_tasks_cleared")
	case reason == postResultWaitBackgroundTask && !since.IsZero() && time.Since(since) >= a.EffectiveHangGrace(postResultGrace):
		waited := time.Since(since)
		m.logger.Warn("agent.headless.post_result_hang", "id", a.ID,
			"reason", reason,
			"wait_ms", waited.Milliseconds(),
			"background_tasks_pending", a.HasBackgroundTasks())
	case reason == postResultWaitForkSubagent && a.TerminalResultIdle(postResultGrace):
		m.logger.Warn("agent.headless.post_result_hang", "id", a.ID,
			"reason", reason,
			"wait_ms", time.Since(a.GetLastEventAt()).Milliseconds())
	default:
		return false
	}
	a.setCompletedByResult(true)
	m.signalKill(a)
	waitExit()
	return true
}

func (m *Manager) maybeArmPipePostResultClose(a *Agent, observedAt time.Time, resolution string) {
	if a.isDetached() || a.cancel == nil {
		return
	}
	reason, _, ok := a.PostResultWait()
	if !ok {
		return
	}
	if reason == postResultWaitBackgroundTask && a.HasBackgroundTasks() {
		return
	}
	delay := postResultFastCloseDelay
	if reason == postResultWaitForkSubagent {
		delay = postResultGrace
	}
	go func() {
		time.Sleep(delay)
		if a.GetState() == StateStopped || a.WasStopped() {
			return
		}
		currentReason, _, ok := a.PostResultWait()
		if !ok {
			return
		}
		if currentReason != postResultWaitFastClose &&
			currentReason != postResultWaitBackgroundTask &&
			currentReason != postResultWaitForkSubagent {
			return
		}
		if currentReason == postResultWaitBackgroundTask && a.HasBackgroundTasks() {
			return
		}
		if currentReason == postResultWaitForkSubagent && !a.TerminalResultIdle(postResultGrace) {
			return
		}
		if a.GetLastEventAt().After(observedAt) {
			return
		}
		m.logPostResultWaitDone(a, resolution)
		a.setCompletedByResult(true)
		a.cancel()
	}()
}

func (m *Manager) handleMalformedToolResults(a *Agent, event StreamEvent) {
	for i := range event.toolResults {
		diag, ok := classifyMalformedToolCall(event.toolResults[i])
		if !ok {
			continue
		}
		toolName, input, _ := a.ToolUseByID(event.toolResults[i].ToolUseID)
		if toolName == "" {
			toolName = "unknown"
		}
		if a.computeCanSteer() && a.GetMalformedToolCorrectionAttempts() < maxMalformedToolCorrectionAttempts {
			prompt := buildMalformedToolCorrectionPrompt(toolName, input, diag)
			if err := m.sendHeadlessSteerMessage(a, prompt); err == nil {
				a.IncMalformedToolCorrectionAttempts()
				a.NoteMalformedToolCall(event.toolResults[i].ToolUseID, toolName, malformedToolCallOutcomeCorrected)
				m.logger.Warn("agent.headless.tool_call_malformed.corrected",
					"id", a.ID,
					"provider", a.GetProvider(),
					"tool", toolName,
					"tool_use_id", event.toolResults[i].ToolUseID,
					"attempt", a.GetMalformedToolCorrectionAttempts())
				return
			}
		}
		reason := fmt.Sprintf("tool %s rejected malformed input: %s", toolName, textutil.TruncateBytesTrimmed(strings.TrimSpace(diag.ValidationError), malformedToolPromptFieldLimit, "\n... (truncated)"))
		a.NoteMalformedToolCall(event.toolResults[i].ToolUseID, toolName, malformedToolCallOutcomeUnrecoverable)
		a.SetError(malformedToolCallErrorKind, reason)
		m.ReportProviderSignal(a.GetProvider(), providerpkg.Classification{
			Signal:     providerpkg.SignalRateLimit,
			Reason:     malformedToolCallErrorKind,
			RetryAfter: malformedToolFailoverCooldown,
			Source:     providerpkg.CooldownFromConfig,
		})
		m.logger.Warn("agent.headless.tool_call_malformed.unrecoverable",
			"id", a.ID,
			"provider", a.GetProvider(),
			"tool", toolName,
			"tool_use_id", event.toolResults[i].ToolUseID,
			"attempts", a.GetMalformedToolCorrectionAttempts(),
			"reason", reason)
		return
	}
}

// handleHeadlessResult records a terminal result event's stats/session,
// applies the cost guardrail, and — for a steerable headless run — drains or
// closes the stdin transport at this turn boundary. Split out of
// processHeadlessLine to keep it under the package's function-length lint
// budget. Returns false when the caller should stop the stream (cost
// guardrail breach).
func (m *Manager) handleHeadlessResult(ctx context.Context, a *Agent, event StreamEvent) (keepGoing bool) {
	a.AddResultStats(event.SessionID, event.CostUSD, event.InputTokens, event.OutputTokens, event.ReasoningTokens)
	a.AddCacheStats(event.CacheCreationInputTokens, event.CacheReadInputTokens)
	// Copilot's billing unit: premium requests (no USD on the result event).
	if event.PremiumRequests > 0 {
		a.AddPremiumRequests(event.PremiumRequests)
	}
	// Must follow AddCacheStats: cached input dominates a codex run and prices at a tenth of standard. EXC:FILE011:load-bearing-invariant
	costNow := a.BankEstimatedCost()
	costSource := a.CostSource()
	// The terminal result's totals now cover everything the live estimate was
	// tracking for the in-flight turn; reset so the next turn's estimate isn't
	// double-counted on top of the freshly banked totals.
	a.ResetLiveUsage()
	// Codex NDJSON never reports session_id/cost on the result event, so
	// those alone read as an empty/crashed run (this misled diagnosis of
	// the 2026-07-05 stalled-workflow incident, #1559). Omit the
	// meaningless fields for codex and log only the token counts it does
	// report, so a healthy codex completion is distinguishable from a
	// real crash at a glance.
	if a.Provider == providerid.Codex {
		m.logger.Info("agent.headless.result", "id", a.ID,
			"input_tokens", event.InputTokens, "output_tokens", event.OutputTokens, "reasoning_tokens", event.ReasoningTokens)
	} else {
		m.logger.Info("agent.headless.result", "id", a.ID, "session_id", event.SessionID, "cost", costNow,
			"input_tokens", event.InputTokens, "output_tokens", event.OutputTokens, "reasoning_tokens", event.ReasoningTokens)
	}
	m.warnIfResultHasLiveBackgroundTasks(a)
	// Persist the captured session ID so a reattach or restart-stale
	// recovery can pass --resume. No-op when survival is disabled.
	if event.SessionID != "" {
		m.saveRegistry(ctx, a)
	}
	m.mu.RLock()
	maxCost := m.guardrails.MaxCostUSD
	m.mu.RUnlock()
	if maxCost > 0 && costNow > maxCost {
		if keepGoing := m.checkCostGuardrail(a, costNow, maxCost, costSource, true, "post_result_usd"); !keepGoing {
			return false
		}
	}

	if resultSubtypeIsError(event.Subtype) || event.ErrorType != "" || event.ErrorStatus != 0 {
		// A queued steer belongs to the next successful turn, not to this failed
		// result. Preserve it for the runner's retry while closing this stdin and
		// rejecting any new input that cannot be delivered by this process.
		m.closeHeadlessSteerAfterError(a)
		a.ClearPostResultWait()
		return true
	}
	m.drainOrCloseHeadlessSteer(a)
	if !shouldTrackPostResultWait(a) {
		a.ClearPostResultWait()
		return true
	}
	switch {
	case a.HasBackgroundTasks():
		a.SetPostResultWait(postResultWaitBackgroundTask, event.Timestamp)
	case a.UsesForkSubagent():
		a.SetPostResultWait(postResultWaitForkSubagent, event.Timestamp)
		m.maybeArmPipePostResultClose(a, a.GetLastEventAt(), "fork_subagent_idle")
	default:
		a.SetPostResultWait(postResultWaitFastClose, event.Timestamp)
		m.maybeArmPipePostResultClose(a, a.GetLastEventAt(), "pipe_fast_close")
	}
	m.saveRegistry(ctx, a)
	return true
}

func (m *Manager) logPostResultWaitDone(a *Agent, resolution string) {
	reason, waited, ok := a.PostResultWaitDuration(time.Now().UTC())
	if !ok {
		return
	}
	m.logger.Info("agent.headless.post_result_close",
		"id", a.ID,
		"reason", reason,
		"resolution", resolution,
		"wait_ms", waited.Milliseconds())
}

// drainOrCloseHeadlessSteer is the steerable headless run's turn-boundary
// chokepoint: it flushes one queued steer message so the process stays alive
// and lands the next turn back-to-back, or marks the transport finalizing.
// Detached FIFO children retain an O_RDWR anchor across restart; their normal
// terminal cleanup is performed by the post-result reaper, not EOF. Gated on
// hasStdinPipe so a non-steerable (legacy one-shot) run is unaffected.
func (m *Manager) drainOrCloseHeadlessSteer(a *Agent) {
	if !a.convo.hasStdinPipe() {
		return
	}
	next, ok := a.PopPendingPromptOrBeginFinalizing()
	if !ok {
		a.convo.closeStdinPipe()
		return
	}
	// Re-gate provider health at this turn boundary, mirroring the persistent
	// Claude path (advanceClaudeTurn): a provider that capped while the current
	// turn ran must not receive the queued steer on its stranded stdin. A
	// headless run has no hot-swap handoff — it recovers by re-dispatch — so an
	// unhealthy provider parks the steer (restore it, close stdin) and marks the
	// run rate-limited so the completion path's reschedule picks it back up
	// instead of writing to a doomed session.
	if !m.providerHealthyForSteer(a) {
		a.RestorePendingPrompt(next)
		m.saveRegistry(m.ctx, a)
		a.SetError("rate_limit", "provider unhealthy at steer boundary")
		m.closeHeadlessSteerForRetry(a)
		m.logger.Warn("agent.headless.steer.parked", "id", a.ID, "provider", a.GetProvider(),
			"remaining", a.PendingPromptCount())
		return
	}
	if writeErr := m.writeUserMessage(a, next); writeErr != nil {
		m.logger.Error("agent.headless.steer.write", "id", a.ID, "err", writeErr)
		a.RestorePendingPrompt(next)
		m.saveRegistry(m.ctx, a)
		m.closeHeadlessSteerForRetry(a)
		return
	}
	m.saveRegistry(m.ctx, a)
	m.logger.Info("agent.headless.steer.flushed", "id", a.ID, "remaining", a.PendingPromptCount())
}

// closeHeadlessSteerAfterError stops the failed stdin turn without consuming
// queued follow-ups. A retry starts a fresh stdin transport and can deliver
// those prompts in order; a new message is rejected rather than falsely
// acknowledged after the terminal error has already arrived.
func (m *Manager) closeHeadlessSteerAfterError(a *Agent) {
	m.closeHeadlessSteerForRetry(a)
}

// closeHeadlessSteerForRetry ends the current transport while retaining queued
// prompts. Detached children retain a FIFO writer across restart, so retry
// paths must explicitly terminate them instead of depending on EOF.
func (m *Manager) closeHeadlessSteerForRetry(a *Agent) {
	a.setFinalizing(true)
	m.saveRegistry(m.ctx, a)
	a.convo.closeStdinPipe()
	// An O_RDWR child intentionally survives the parent's close across a
	// restart. A terminal provider error is different: this attempt must end
	// now so the normal retry path can start a fresh transport.
	if a.isDetached() {
		m.signalKill(a)
	}
}

// sendHeadlessSteerMessage queues a follow-up message for a steerable
// headless claude run. Unlike the conversational turn boundary
// (advanceClaudeTurn), there is no idle state to write into directly — a
// headless agent stays StateRunning for its whole run — so every message is
// queued and flushed at the next "result" event boundary (processHeadlessLine).
// Rejects once the run has begun finalizing (its stdin is being closed for
// good) so a message is never silently queued for a process that is exiting.
func (m *Manager) sendHeadlessSteerMessage(a *Agent, text string) error {
	queueLen, enqueued, finalizing := a.TryEnqueuePrompt(text, MaxPendingHeadlessSteerPrompts)
	if !enqueued {
		if finalizing {
			return conflictError(fmt.Sprintf("agent %s is finalizing and can no longer accept messages", a.ID))
		}
		return conflictError(fmt.Sprintf("agent %s has too many pending steer messages (%d max)", a.ID, MaxPendingHeadlessSteerPrompts))
	}
	m.saveRegistry(m.ctx, a)
	m.logger.Info("agent.headless.message_queued", "id", a.ID, "queue_len", queueLen)

	// Surface the sent message immediately in StreamOutput — the CLI only
	// echoes tool-result/assistant turns back over stdout, never the
	// injected user text itself.
	ev := StreamEvent{Type: "user_input", Content: text, Timestamp: time.Now().UTC()}
	a.AppendOutput(ev)
	m.emit(events.AgentOutput(a.ID), ev)
	return nil
}

// sendHeadlessConvergeSteer delivers the live-cost "wrap up and converge"
// nudge by writing it straight to the running child's stdin instead of parking
// it on the pending-prompt queue. The queue is flushed only by
// drainOrCloseHeadlessSteer at a terminal "result" boundary, but the live-cost
// ceiling exists precisely for the case where a single in-flight turn's own
// usage crosses both the steer threshold and the hard-stop ceiling before any
// result event lands — a queued message would then be killed with the process,
// never reaching the model. Writing it in-band (stream-json input mode buffers
// it) hands the CLI the instruction at the earliest turn boundary the run
// reaches. It is best-effort: a turn that blows straight past the hard ceiling
// still cannot be interrupted mid-generation. Bypassing the queue also avoids
// a double delivery, since drainOrCloseHeadlessSteer will find nothing to pop.
func (m *Manager) sendHeadlessConvergeSteer(a *Agent, text string) error {
	if a.isFinalizing() {
		return conflictError(fmt.Sprintf("agent %s is finalizing and can no longer accept messages", a.ID))
	}
	if !a.convo.hasStdinPipe() {
		return conflictError(fmt.Sprintf("agent %s has no live stdin transport for a converge steer", a.ID))
	}
	if err := m.writeUserMessage(a, text); err != nil {
		return err
	}
	m.saveRegistry(m.ctx, a)
	m.logger.Info("agent.headless.converge_steer_sent", "id", a.ID)

	// Surface the sent message in StreamOutput — the CLI only echoes
	// tool-result/assistant turns back over stdout, never the injected text.
	ev := StreamEvent{Type: "user_input", Content: text, Timestamp: time.Now().UTC()}
	a.AppendOutput(ev)
	m.emit(events.AgentOutput(a.ID), ev)
	return nil
}

// warnIfResultHasLiveBackgroundTasks logs a warning when a terminal result
// event arrives while Sybra's last known state still shows a live CLI
// background bash task. A headless process exits as soon as its final turn
// ends and tears down any such task at that point — Sybra cannot make an
// already-exiting process wait longer. A task killed mid-write (e.g. `npm
// ci` extracting packages) leaves the worktree silently corrupted, which
// then fails a later step (verify_checks, build) deterministically and gets
// misdiagnosed as a code defect instead of an infra one (see task
// 3aeabb65). The result event is the last point Sybra observes the agent's
// own view of its live tasks, so it's the only place this can be caught.
func (m *Manager) warnIfResultHasLiveBackgroundTasks(a *Agent) {
	if !a.HasBackgroundTasks() {
		return
	}
	m.logger.Warn("agent.headless.result_with_live_background_tasks", "id", a.ID,
		"hint", "headless turn ended while a background bash task was still running; it will be killed with the process, which can leave partial writes in the worktree")
}

// checkCostGuardrail hard-stops a headless run on a breach of MaxCostUSD.
// Unlike checkTurnsGuardrail there is no auto-continue or human-continue path:
// cost is the hard ceiling turns' auto-continue is itself gated against, so a
// breach must terminate the subprocess immediately instead of leaving a
// headless process alive while waiting for a response. Returns false so the
// caller stops the stream and kills/cancels the subprocess.
//
// This is only ever called from handleHeadlessResult on a "result" event, so
// the breach always coincides with a turn that already finished cleanly on
// its own — there is no more work the subprocess could still do, only an
// idle process left to reap. Mark completedByResult so the attempt-exit path
// (wasCompletedByResult check in runHeadlessAttemptPipe/Survive) derives the
// outcome from that terminal result event instead of stamping
// errCostGuardrailExceeded — otherwise a legitimately completed turn (and
// any sidecar it already wrote) gets discarded as a hard failure purely
// because the kill happened to land after the cost ceiling (see task
// 6ee7ee8d).
func (m *Manager) checkCostGuardrail(a *Agent, costNow, maxCost float64, costSource string, completedByResult bool, measurement string) bool {
	m.logger.Warn("agent.guardrail.cost", "id", a.ID, "cost", costNow, "limit", maxCost, "source", costSource, "measurement", measurement)
	a.MarkStopped()
	// Stamping "cost" over a checkpoint discards a handoff whose work is already committed. EXC:FILE011:load-bearing-invariant
	if !IsCheckpointEscalation(a.GetEscalationReason()) {
		a.SetEscalationReason(EscalationReasonCost)
	}
	a.setCompletedByResult(completedByResult)
	m.emit(events.AgentEscalation(a.ID), EscalationEvent{
		Reason:        "cost",
		Guardrail:     "execution.agent.post_result_cost_usd",
		Measurement:   measurement,
		CostSource:    costSource,
		CostUSD:       costNow,
		MeasuredValue: costNow,
		Limit:         maxCost,
	})
	m.emit(events.AgentState(a.ID), a)
	return false
}

// applyAssistantGuardrails accounts an assistant event's token usage and runs
// the per-turn guardrails. Returns true when the caller should stop the stream.
func (m *Manager) applyAssistantGuardrails(ctx context.Context, a *Agent, event StreamEvent) bool {
	// Copilot reports output tokens per assistant.message (claude/codex carry 0
	// here and total on the result event instead). Accumulate them as they
	// stream so the stats reflect a copilot run that has no token totals on its
	// terminal result.
	if event.OutputTokens > 0 {
		a.AddOutputTokens(event.OutputTokens)
	}
	// A forked subagent's assistant turns carry parent_tool_use_id and must not
	// inflate the top-level turn count: a single Task-tool fan-out can emit
	// hundreds of subagent turns that have nothing to do with the top-level
	// conversation the guardrail is meant to bound. They are bounded by their
	// own separate ceiling instead (checkSubagentTurnsGuardrail).
	if event.parentToolUseID == "" {
		if stop := m.maybeEnforceLiveCostCeiling(a, event); stop {
			return true
		}
		return !m.checkTurnsGuardrail(ctx, a)
	}
	// Fork subagents get no steer/turn nudge (they are bounded by their own turn
	// ceiling), but their token usage must still feed the live cost estimate so
	// a runaway fan-out is stopped as it spends.
	if stop := m.bankLiveUsageAndCheckCostCeiling(a, event); stop {
		return true
	}
	return !m.checkSubagentTurnsGuardrail(a)
}

// maybeEnforceLiveCostCeiling banks a top-level assistant event's own token
// usage into the run's in-flight live estimate and, once it breaches
// MaxCostUSD, hard-stops the stream before the terminal result event would
// have reported a cost that is already fully incurred (see the doc on
// LiveCostEstimateUSD/BankEstimatedCost for why providers can't do this
// themselves). Below the ceiling but at or above TurnCostFraction of it, it
// nudges the run with a one-shot steer message asking it to converge, giving
// it a chance to land cleanly rather than being cut off mid-turn. Returns true
// when the caller should stop the stream.
func (m *Manager) maybeEnforceLiveCostCeiling(a *Agent, event StreamEvent) bool {
	if stop := m.bankLiveUsageAndCheckCostCeiling(a, event); stop {
		return true
	}
	m.mu.RLock()
	maxCost := m.guardrails.MaxCostUSD
	fraction := m.guardrails.TurnCostFraction
	m.mu.RUnlock()
	if maxCost <= 0 {
		return false
	}
	if fraction <= 0 {
		fraction = 0.8
	}
	costNow := a.LiveCostEstimateUSD()
	if costNow >= fraction*maxCost && a.computeCanSteer() && !a.MarkBudgetSteerSent() {
		msg := fmt.Sprintf(
			"You are approaching the run's cost ceiling (estimated $%.2f of a $%.2f limit). "+
				"Please wrap up your current work and converge to a stopping point soon — the run will be stopped once the ceiling is reached.",
			costNow, maxCost)
		if err := m.sendHeadlessConvergeSteer(a, msg); err != nil {
			m.logger.Warn("agent.guardrail.cost.steer_failed", "id", a.ID, "err", err)
		}
	}
	return false
}

// bankLiveUsageAndCheckCostCeiling banks an assistant event's own token usage
// into the run's in-flight live estimate and hard-stops the stream once that
// pushes the run over MaxCostUSD. It is called for both top-level and forked
// -subagent assistant turns: a runaway fork fan-out multiplies spend across
// hundreds of subagent turns (CLAUDE.md: "total cost multiplies with
// parallelism"), and those turns never reach the top-level steer/turn
// guardrails — so without banking them here the live ceiling would be blind to
// exactly the spend it exists to bound, catching it only reactively once/if a
// terminal result event eventually lands. Returns true when the caller should
// stop the stream.
func (m *Manager) bankLiveUsageAndCheckCostCeiling(a *Agent, event StreamEvent) bool {
	a.AddLiveUsage(event.InputTokens, event.OutputTokens, event.CacheCreationInputTokens, event.CacheReadInputTokens)
	m.mu.RLock()
	maxCost := m.guardrails.MaxCostUSD
	m.mu.RUnlock()
	if maxCost <= 0 {
		return false
	}
	costNow := a.LiveCostEstimateUSD()
	if costNow > maxCost {
		m.checkCostGuardrail(a, costNow, maxCost, "estimated", false, "live_estimate_usd")
		return true
	}
	return false
}

// checkSubagentTurnsGuardrail increments the forked-subagent turn counter
// and, on a breach of effectiveMaxSubagentEvents, hard-stops the run.
// Unlike checkTurnsGuardrail there is no auto-continue or human-escalation
// path: a runaway subagent fan-out is stopped outright. Returns false when
// the caller should stop the stream.
func (m *Manager) checkSubagentTurnsGuardrail(a *Agent) bool {
	count := a.IncSubagentTurnCount()
	maxEvents := m.effectiveMaxSubagentEvents()
	if maxEvents <= 0 || count < maxEvents {
		return true
	}
	m.logger.Warn("agent.guardrail.subagent_turns", "id", a.ID, "count", count, "limit", maxEvents)
	a.MarkStopped()
	a.SetEscalationReason(EscalationReasonSubagentTurns)
	m.emit(events.AgentEscalation(a.ID), EscalationEvent{
		Reason:        "subagent_turns",
		Guardrail:     "execution.agent.max_subagent_events",
		Measurement:   "subagent_events",
		TurnCount:     count,
		MeasuredValue: float64(count),
		Limit:         float64(maxEvents),
	})
	m.emit(events.AgentState(a.ID), a)
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
		if m.canCheckpointOnTurnCeiling(a) {
			return m.checkpointAndHandoff(ctx, a, turns, maxTurns)
		}
		multiplier := m.effectiveTurnMultiplier()
		newLimit := int(float64(maxTurns) * multiplier)
		a.SetMaxTurns(newLimit)
		m.logger.Info("agent.guardrail.turns.auto_continued", "id", a.ID, "turns", turns, "new_limit", newLimit)
		return true
	}
	a.SetEscalationReason(EscalationReasonTurns)
	m.emit(events.AgentEscalation(a.ID), EscalationEvent{
		Reason:        "turns",
		Guardrail:     "execution.agent.max_assistant_events",
		Measurement:   "assistant_events",
		TurnCount:     turns,
		MeasuredValue: float64(turns),
		Limit:         float64(maxTurns),
	})
	m.emit(events.AgentState(a.ID), a)
	select {
	case continueRun := <-a.escalationCh:
		if !continueRun {
			// Caller terminates the subprocess: streamHeadlessOutput cancels
			// the context (pipe-backed); tailHeadlessFile signal-kills the
			// detached child by PID.
			a.MarkStopped()
			return false
		}
		a.SetEscalationReason("")
		m.emit(events.AgentState(a.ID), a)
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *Manager) checkpointAndHandoff(ctx context.Context, a *Agent, turns, maxTurns int) bool {
	worktreeDir := strings.TrimSpace(a.sessionCWD)
	if worktreeDir == "" {
		a.SetEscalationReason(EscalationReasonCheckpointFailed)
		a.MarkStopped()
		a.setCompletedByResult(true)
		m.logger.Error("agent.guardrail.turns.checkpoint_failed",
			"id", a.ID, "turns", turns, "limit", maxTurns, "err", "missing worktree dir")
		m.emit(events.AgentState(a.ID), a)
		return false
	}

	msg := fmt.Sprintf("chore(checkpoint): preserve progress at turn %d\n\nSybra checkpointed this run at the per-run turn ceiling (%d turns) before handing off to a fresh agent.", turns, maxTurns)
	committed, err := project.CheckpointCommit(ctx, worktreeDir, msg)
	if err != nil {
		a.SetEscalationReason(EscalationReasonCheckpointFailed)
		a.MarkStopped()
		a.setCompletedByResult(true)
		m.logger.Error("agent.guardrail.turns.checkpoint_failed",
			"id", a.ID, "turns", turns, "limit", maxTurns, "dir", worktreeDir, "err", err)
		m.emit(events.AgentState(a.ID), a)
		return false
	}

	a.SetEscalationReason(EscalationReasonCheckpoint)
	a.MarkStopped()
	a.setCompletedByResult(true)
	m.logger.Info("agent.guardrail.turns.checkpoint",
		"id", a.ID, "turns", turns, "limit", maxTurns, "dir", worktreeDir, "committed", committed)
	m.emit(events.AgentState(a.ID), a)
	return false
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

func (m *Manager) handleError(ctx context.Context, a *Agent, err error) {
	kind := classifyAgentError(err)
	a.SetError(kind, err.Error())
	a.SetState(StateStopped)
	m.logger.Error("agent.error", "id", a.ID, "kind", kind, "err", err)
	m.emit(events.AgentError(a.ID), ErrorEvent{Kind: kind, Msg: err.Error()})
	m.emit(events.AgentState(a.ID), a)
	m.fireComplete(ctx, a, false)
	m.markAgentDone(ctx, a)
}

// classifyAgentError maps a fatal agent error to a canonical kind string.
func classifyAgentError(err error) string {
	if err == nil {
		return "crash"
	}
	// Ahead of the substring table, which reads this as a bare i/o timeout and
	// mislabels it git_clone.
	if errors.Is(err, errPromptUndelivered) {
		return ErrorKindPromptUndelivered
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "worktree") || strings.Contains(msg, "already checked out"):
		return "worktree_conflict"
	case errclass.Classify(msg, errclass.AgentRecoveryBiased) == errclass.Transient:
		return "git_clone"
	case strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "eacces") ||
		strings.Contains(msg, "operation not permitted"):
		return "permission_denied"
	case errclass.Classify(msg, errclass.AgentRecoveryBiased) == errclass.RateLimited:
		return "rate_limit"
	default:
		return "crash"
	}
}
