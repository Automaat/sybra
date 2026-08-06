package provider

import (
	"strings"
	"testing"
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
