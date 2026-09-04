package provider

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

// A provider usage cap arrives on a subtype:"success" result with the limit
// text as essentially the whole content, so clean-result classification has to
// stay. What it must not do is fire on an agent's own answer that discusses
// limits — which is exactly what an agent working on this file writes.
func TestCleanResultContent_DistinguishesProviderCapFromAgentProse(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Signal
	}{
		{
			// The real shape: short, and the message is the whole result.
			name:    "provider cap notice is still classified",
			content: "You've hit your usage limit. Your limit will reset at 5pm.",
			want:    SignalRateLimit,
		},
		{
			name:    "session limit notice is still classified",
			content: "You've hit your session limit · resets 4:30pm",
			want:    SignalRateLimit,
		},
		{
			// An agent reporting on work it did in this area. Long, with the
			// phrase incidental.
			name: "agent prose about limits is not a rate limit",
			content: "Done. I implemented the reset-hint parser for the case where codex says " +
				"you've hit your usage limit and prints a reset time. The parser now rejects " +
				"out-of-range instants rather than clamping them, because clamping turned a " +
				"misparse into the worst possible park. I also added regression tests covering " +
				"the decoy case, the malformed-year case, and the ANSI-wrapped form, and checked " +
				"each by reverting the fix to confirm the test fails.",
			want: SignalNone,
		},
		{
			// Long output that merely quotes a limit line, e.g. a tool result
			// echoed back inside a longer answer.
			name:    "long answer quoting a limit line is not a rate limit",
			content: strings.Repeat("analysis of the failing verify suite. ", 20) + "rate limit exceeded",
			want:    SignalNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyClaudeError(ErrorSample{Content: tc.content, ContentIsCleanResult: true})
			if got.Signal != tc.want {
				t.Errorf("signal = %v, want %v (content %d chars)", got.Signal, tc.want, len(tc.content))
			}
		})
	}
}

// A non-clean result is a run that actually failed, so its content keeps the
// broad needle matching regardless of length.
func TestNonCleanResultContent_UnaffectedByLengthBudget(t *testing.T) {
	long := strings.Repeat("stack trace line\n", 50) + "rate limit exceeded"
	got := ClassifyClaudeError(ErrorSample{Content: long})
	if got.Signal != SignalRateLimit {
		t.Errorf("signal = %v, want SignalRateLimit — a failed run's content is still evidence", got.Signal)
	}
}

// The auth needles have the same problem the limit needles had: "not logged
// in" is exactly what an agent writes when it works on a login path. A run the
// provider served is not a refusal, and a refusal is one terse line.
func TestAuthContent_DistinguishesProviderRefusalFromAgentProse(t *testing.T) {
	prose := "I finished the change to the login error path. The handler previously " +
		"returned a bare 401 with no body, so a caller could not tell an expired token " +
		"from a missing one. It now writes the message \"not logged in\" alongside the " +
		"status, and the tests cover both branches plus the refresh path. Nothing else " +
		"in the package changed, and the existing callers keep compiling."
	classifiers := map[string]func(ErrorSample) Classification{
		providerid.Claude:   ClassifyClaudeError,
		providerid.Codex:    ClassifyCodexError,
		providerid.Copilot:  ClassifyCopilotError,
		providerid.OpenCode: ClassifyOpenCodeError,
	}
	for name, classify := range classifiers {
		t.Run(name+"_clean_run", func(t *testing.T) {
			got := classify(ErrorSample{Content: prose, ContentIsAgentMessage: true, ContentIsCleanResult: true})
			if got.Signal == SignalAuthFailure {
				t.Fatalf("a run the provider served was classified as an auth failure: %+v", got)
			}
		})
		t.Run(name+"_failed_run_long_report", func(t *testing.T) {
			got := classify(ErrorSample{Content: prose, ContentIsAgentMessage: true})
			if got.Signal == SignalAuthFailure {
				t.Fatalf("an agent report was classified as an auth failure: %+v", got)
			}
		})
	}
}

func TestAuthContent_TerseRefusalStillClassifies(t *testing.T) {
	cases := map[string]struct {
		classify func(ErrorSample) Classification
		content  string
	}{
		providerid.Claude:   {ClassifyClaudeError, "Not logged in · Please run /login"},
		providerid.Codex:    {ClassifyCodexError, "Not logged in. Please run: codex login"},
		providerid.Copilot:  {ClassifyCopilotError, "Not logged in. Please run: copilot login"},
		providerid.OpenCode: {ClassifyOpenCodeError, "Invalid API key. Please run: opencode auth login"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.classify(ErrorSample{Content: tc.content, ContentIsAgentMessage: true})
			if got.Signal != SignalAuthFailure {
				t.Fatalf("a terse provider refusal was not classified as an auth failure: %+v", got)
			}
		})
	}
}

func TestAuthContent_StderrRefusalIgnoresContentLength(t *testing.T) {
	long := strings.Repeat("the agent wrote a long report about the login path. ", 40)
	got := ClassifyClaudeError(ErrorSample{Stderr: "not logged in", Content: long})
	if got.Signal != SignalAuthFailure {
		t.Fatalf("a refusal on the CLI's own channel was ignored: %+v", got)
	}
}

// llmexec classifies against the provider CLI's whole stdout, not a terse
// result line, and only fails over to a peer when the signal is not None. A
// refusal buried in that envelope has to keep classifying or a logged-out
// first provider hard-fails the job instead of falling over.
func TestAuthContent_RefusalInsideACliEnvelopeStillClassifies(t *testing.T) {
	envelopes := map[string]struct {
		classify func(ErrorSample) Classification
		blob     string
	}{
		providerid.Claude: {ClassifyClaudeError, `{"type":"result","subtype":"error_during_execution",` +
			`"is_error":true,"duration_ms":412,"num_turns":0,"session_id":"3f2b1c9a-7d44-4e10-9c33-8a1f2e6b5d70",` +
			`"result":"Not logged in · Please run /login","total_cost_usd":0,"usage":{"input_tokens":0,` +
			`"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`},
		providerid.Codex: {ClassifyCodexError, `{"id":"0","msg":{"type":"session_configured","session_id":` +
			`"019bd4f0-8c21-7a3e-9f55-6d2c8b4e1a90","model":"gpt-5.6","history_log_id":0}}` + "\n" +
			`{"id":"0","msg":{"type":"error","message":"Not logged in. Please run: codex login"}}` + "\n" +
			`{"id":"0","msg":{"type":"task_complete","last_agent_message":null}}`},
		providerid.Copilot: {ClassifyCopilotError, `{"error":{"type":"auth","code":"unauthenticated","message":` +
			`"Not logged in. Please run: copilot login"},"data":null,"request_id":` +
			`"7c1f0b2a-53de-4a88-9b17-2e6d4c8f0a35","usage":{"premium_requests":0,"input_tokens":0,` +
			`"output_tokens":0},"model":"claude-sonnet-4.5"}`},
		providerid.OpenCode: {ClassifyOpenCodeError, `{"error":{"name":"AuthError","data":{"providerID":` +
			`"anthropic","message":"Invalid API key. Please run: opencode auth login"}},"parts":[],` +
			`"sessionID":"ses_6b2f9c1d0","messageID":"msg_4a7e3f style","tokens":{"input":0,"output":0,` +
			`"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}`},
	}
	for name, tc := range envelopes {
		t.Run(name, func(t *testing.T) {
			if len(tc.blob) < 200 {
				t.Fatalf("envelope is %d chars, too short to exercise a realistic blob", len(tc.blob))
			}
			got := tc.classify(ErrorSample{Content: strings.ToLower(tc.blob)})
			if got.Signal != SignalAuthFailure {
				t.Fatalf("a refusal inside a %d-char CLI envelope was dropped, so the job cannot fail over: %+v", len(tc.blob), got)
			}
		})
	}
}

// An agent that quotes a provider's refusal verbatim in its report is still
// reporting, not being refused — the run the provider served proves it.
func TestAuthContent_ServedRunQuotingARefusalIsNotOne(t *testing.T) {
	cases := map[string]struct {
		classify func(ErrorSample) Classification
		content  string
	}{
		providerid.Claude:   {ClassifyClaudeError, "no change needed: the cli already prints \"not logged in · please run /login\""},
		providerid.Codex:    {ClassifyCodexError, "no change needed: the cli already prints \"not logged in. please run: codex login\""},
		providerid.Copilot:  {ClassifyCopilotError, "no change needed: the cli already prints \"please run: copilot login\""},
		providerid.OpenCode: {ClassifyOpenCodeError, "no change needed: the cli already prints \"invalid api key\""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.classify(ErrorSample{Content: tc.content, ContentIsAgentMessage: true, ContentIsCleanResult: true})
			if got.Signal == SignalAuthFailure {
				t.Fatalf("a run the provider served was read as a refusal: %+v", got)
			}
		})
	}
}

func TestOpenCodeQuotaContent_DoesNotParkOnItsOwnProse(t *testing.T) {
	prose := "I reviewed the quota handling. The client now reports a rate limit " +
		"distinctly from an exhausted quota, and the retry path waits on the reset " +
		"header rather than a fixed delay. Tests cover both branches."
	got := ClassifyOpenCodeError(ErrorSample{Content: prose, ContentIsAgentMessage: true, ContentIsCleanResult: true})
	if got.Signal == SignalRateLimit {
		t.Fatalf("a served run was parked off its own prose: %+v", got)
	}
	refused := ClassifyOpenCodeError(ErrorSample{Content: "rate limit exceeded, retry later"})
	if refused.Signal != SignalRateLimit {
		t.Fatalf("a real rate-limit refusal was dropped: %+v", refused)
	}
}

// The two version-only providers are never told by their probe that they are
// logged out, so a refusal that arrives on a run's stdout is the only signal
// there is. An envelope carrying no instruction text must still classify, or
// they stay healthy and every dispatch burns a request with no failover.
func TestAuthContent_InstructionFreeEnvelopeStillClassifies(t *testing.T) {
	cases := map[string]struct {
		classify func(ErrorSample) Classification
		blob     string
	}{
		providerid.Copilot: {ClassifyCopilotError, `{"error":{"type":"auth","code":"unauthenticated",` +
			`"message":"you are not logged in."},"data":null,"request_id":"7c1f0b2a-53de-4a88-9b17-2e6d4c8f0a35",` +
			`"usage":{"premium_requests":0,"input_tokens":0,"output_tokens":0}}`},
		providerid.OpenCode: {ClassifyOpenCodeError, `{"error":{"name":"providerautherror","data":{"message":` +
			`"ai_apicallerror: unauthorized"}},"parts":[],"sessionid":"ses_6b2f9c1d0","tokens":{"input":0,` +
			`"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}`},
		providerid.Codex: {ClassifyCodexError, `{"id":"0","msg":{"type":"session_configured","session_id":` +
			`"019bd4f0-8c21-7a3e-9f55-6d2c8b4e1a90","model":"gpt-5.6"}}` + "\n" +
			`{"id":"0","msg":{"type":"error","message":"not logged in"}}`},
		providerid.Claude: {ClassifyClaudeError, `{"type":"result","subtype":"error_during_execution",` +
			`"is_error":true,"session_id":"3f2b1c9a-7d44-4e10-9c33-8a1f2e6b5d70",` +
			`"result":"not logged in","total_cost_usd":0}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.classify(ErrorSample{Content: tc.blob})
			if got.Signal != SignalAuthFailure {
				t.Fatalf("a refusal with no instruction text was dropped from a CLI envelope: %+v", got)
			}
		})
	}
}
