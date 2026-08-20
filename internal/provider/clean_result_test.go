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
			got := classify(ErrorSample{Content: prose, ContentIsCleanResult: true})
			if got.Signal == SignalAuthFailure {
				t.Fatalf("a run the provider served was classified as an auth failure: %+v", got)
			}
		})
		t.Run(name+"_failed_run_long_report", func(t *testing.T) {
			got := classify(ErrorSample{Content: prose})
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
		providerid.Copilot:  {ClassifyCopilotError, "not logged in"},
		providerid.OpenCode: {ClassifyOpenCodeError, "not authenticated"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.classify(ErrorSample{Content: tc.content})
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
