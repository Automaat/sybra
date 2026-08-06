package workflow

import "testing"

// The judge parser used to scan from the first `{` to the last `}` with no
// string-literal tracking, so a brace inside a rationale — which is prose the
// model writes about code — sliced a malformed span and failed the step on
// output the shared scanner reads fine.
func TestExtractJudgeJSONToleratesBracesAndProse(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantWinner string
	}{
		{
			name:       "bare object",
			output:     `{"winner_attempt_id":"a1","scores":[{"attempt_id":"a1","score":0.9}],"rationale":"clean"}`,
			wantWinner: "a1",
		},
		{
			name:       "close brace inside the rationale",
			output:     `{"winner_attempt_id":"a1","scores":[],"rationale":"a2 leaves a stray } in the diff"}`,
			wantWinner: "a1",
		},
		{
			name:       "prose with braces around the object",
			output:     "The blocks `{` and `}` are unbalanced in a2.\n```json\n" + `{"winner_attempt_id":"a1","scores":[],"rationale":"ok"}` + "\n```",
			wantWinner: "a1",
		},
		{
			// An odd ASCII quote in the prose flips the balanced scanner's
			// string-literal state and hides the object entirely. Losing this
			// discards every attempt in the round.
			name:       "odd quote in the prose",
			output:     "Attempt a2 hardcodes a 6\" margin.\n" + `{"winner_attempt_id":"a1","scores":[],"rationale":"ok"}`,
			wantWinner: "a1",
		},
		{
			name:       "unterminated string literal mentioned in prose",
			output:     "a2 leaves an unterminated string literal (\") in parser.go.\n" + `{"winner_attempt_id":"a1","scores":[],"rationale":"ok"}`,
			wantWinner: "a1",
		},
		{
			name:       "decoy verdict before the real one",
			output:     `{"winner_attempt_id":"a2","scores":[]}` + "\nOn reflection:\n" + `{"winner_attempt_id":"a1","scores":[],"rationale":"revised"}`,
			wantWinner: "a1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractJudgeJSON(tt.output)
			if err != nil {
				t.Fatalf("extractJudgeJSON: %v", err)
			}
			if got.WinnerAttemptID != tt.wantWinner {
				t.Errorf("winner = %q, want %q", got.WinnerAttemptID, tt.wantWinner)
			}
		})
	}
}

func TestExtractJudgeJSONRejectsOutputWithNoObject(t *testing.T) {
	if _, err := extractJudgeJSON("the judge refused to answer"); err == nil {
		t.Error("expected an error on output containing no JSON object")
	}
}
