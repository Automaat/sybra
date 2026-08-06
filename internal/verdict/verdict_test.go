package verdict

import (
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		input      string
		want       Decision
		wantSource Source
		wantErr    bool
	}{
		{
			name:       "bare json human",
			input:      `{"decision":"human","reason":"needs scope clarification","recoverable_action":"none","confidence":"high"}`,
			want:       Decision{Decision: "human", Reason: "needs scope clarification", Summary: "needs scope clarification", RecoverableAction: "none", Confidence: "high"},
			wantSource: SourceJSON,
		},
		{
			name: "bare json sybra_bug",
			input: `{"decision":"sybra_bug","reason":"verify_commits flipped despite push","recoverable_action":"none","confidence":"medium",` +
				`"issue_title":"fix(workflow): verify_commits race","issue_body":"## What\nrace","issue_labels":["workflow"]}`,
			want: Decision{
				Decision: "sybra_bug", Reason: "verify_commits flipped despite push", Summary: "verify_commits flipped despite push",
				RecoverableAction: "none", Confidence: "medium",
				IssueTitle: "fix(workflow): verify_commits race", IssueBody: "## What\nrace",
				IssueLabels: []string{"workflow"},
			},
			wantSource: SourceJSON,
		},
		{
			name:       "bare json with trailing prose",
			input:      `{"decision":"human","reason":"ok","recoverable_action":"none","confidence":"medium"}` + "\n\nThanks for checking!",
			want:       Decision{Decision: "human", Reason: "ok", Summary: "ok", RecoverableAction: "none", Confidence: "medium"},
			wantSource: SourceJSON,
		},
		{
			name: "fenced fallback when no bare json",
			input: "Looks like a real ambiguity.\n\n```sybra-verdict\n" +
				`{"decision":"human","summary":"needs scope clarification"}` +
				"\n```\n",
			want:       Decision{Decision: "human", Reason: "needs scope clarification", Summary: "needs scope clarification", RecoverableAction: "none", Confidence: "medium"},
			wantSource: SourceFence,
		},
		{
			name: "bare json precedence over fenced block",
			input: `{"decision":"human","reason":"bare wins","recoverable_action":"none","confidence":"medium"}` +
				"\n\n```sybra-verdict\n" +
				`{"decision":"sybra_bug","summary":"fenced loses"}` +
				"\n```\n",
			want:       Decision{Decision: "human", Reason: "bare wins", Summary: "bare wins", RecoverableAction: "none", Confidence: "medium"},
			wantSource: SourceJSON,
		},
		{
			name:       "case-mismatched decision normalizes",
			input:      `{"decision":"HUMAN","reason":"still valid","recoverable_action":"none","confidence":"LOW"}`,
			want:       Decision{Decision: "human", Reason: "still valid", Summary: "still valid", RecoverableAction: "none", Confidence: "low"},
			wantSource: SourceJSON,
		},
		{
			name:       "unblocked decision valid",
			input:      `{"decision":"unblocked","reason":"fixed lint, pushed, advanced to ready-pr","recoverable_action":"ready-pr","confidence":"high"}`,
			want:       Decision{Decision: "unblocked", Reason: "fixed lint, pushed, advanced to ready-pr", Summary: "fixed lint, pushed, advanced to ready-pr", RecoverableAction: "ready-pr", Confidence: "high"},
			wantSource: SourceJSON,
		},
		{
			name:       "legacy summary defaults action and confidence",
			input:      `{"decision":"human","summary":"legacy output"}`,
			want:       Decision{Decision: "human", Reason: "legacy output", Summary: "legacy output", RecoverableAction: "none", Confidence: "medium"},
			wantSource: SourceJSON,
		},
		{
			name:    "invalid decision value",
			input:   `{"decision":"maybe","reason":"x","recoverable_action":"none","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name:    "empty summary",
			input:   `{"decision":"human","reason":"","recoverable_action":"none","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name:    "whitespace summary",
			input:   `{"decision":"human","reason":"   ","recoverable_action":"none","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name:    "issue_labels wrong type",
			input:   `{"decision":"sybra_bug","reason":"x","recoverable_action":"none","confidence":"medium","issue_labels":[1,2,3]}`,
			wantErr: true,
		},
		{
			name:  "issue_labels trimmed and blanks dropped",
			input: `{"decision":"sybra_bug","reason":"x","recoverable_action":"none","confidence":"medium","issue_labels":[" workflow ","",  "  ","bug"]}`,
			want: Decision{
				Decision: "sybra_bug", Reason: "x", Summary: "x",
				RecoverableAction: "none", Confidence: "medium",
				IssueLabels: []string{"workflow", "bug"},
			},
			wantSource: SourceJSON,
		},
		{
			name:    "invalid recoverable action",
			input:   `{"decision":"unblocked","reason":"x","recoverable_action":"blocked","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name:    "non-dispatchable recovery action",
			input:   `{"decision":"unblocked","reason":"x","recoverable_action":"planning","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name:    "invalid confidence",
			input:   `{"decision":"human","reason":"x","recoverable_action":"none","confidence":"certain"}`,
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no fenced block, not json",
			input:   "no verdict here at all",
			wantErr: true,
		},
		{
			name:    "broken fenced json",
			input:   "```sybra-verdict\n{broken\n```",
			wantErr: true,
		},
		{
			name:    "unknown decision in fenced block",
			input:   "```sybra-verdict\n{\"decision\":\"maybe\"}\n```",
			wantErr: true,
		},
		{
			name:    "placeholder summary fails closed",
			input:   `{"decision":"sybra_bug","reason":"test","recoverable_action":"none","confidence":"medium","issue_title":"test title","issue_body":"test body"}`,
			wantErr: true,
		},
		{
			name:    "placeholder summary case-insensitive fails closed",
			input:   `{"decision":"human","reason":"  Test  ","recoverable_action":"none","confidence":"medium"}`,
			wantErr: true,
		},
		{
			name: "placeholder issue title fails closed even with real summary",
			input: `{"decision":"sybra_bug","reason":"verify_commits flipped despite push","recoverable_action":"none","confidence":"medium",` +
				`"issue_title":"test title","issue_body":"real body"}`,
			wantErr: true,
		},
		{
			name: "summary merely mentioning test is not a placeholder",
			input: `{"decision":"sybra_bug","reason":"the test suite is flaky","recoverable_action":"none","confidence":"medium",` +
				`"issue_title":"fix(ci): flaky test","issue_body":"real body"}`,
			want: Decision{
				Decision: "sybra_bug", Reason: "the test suite is flaky", Summary: "the test suite is flaky",
				RecoverableAction: "none", Confidence: "medium",
				IssueTitle: "fix(ci): flaky test", IssueBody: "real body",
			},
			wantSource: SourceJSON,
		},
		{
			name:    "envelope-shaped input fails closed",
			input:   `{"structured_output":{"decision":"human","summary":"nested, should not be found"}}`,
			wantErr: true,
		},
		{
			name:    "result-wrapped envelope fails closed",
			input:   `{"result":{"decision":"sybra_bug","summary":"nested"}}`,
			wantErr: true,
		},
		{
			name:    "schema placeholder sybra bug fails closed",
			input:   `{"decision":"sybra_bug","reason":"test","recoverable_action":"none","confidence":"medium","issue_title":"test title","issue_body":"test body"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, src, err := Parse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v (source=%s)", got, src)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.Decision != tc.want.Decision || got.Summary != tc.want.Summary || got.Reason != tc.want.Reason {
				t.Errorf("decision/summary: got %+v want %+v", got, tc.want)
			}
			if got.RecoverableAction != tc.want.RecoverableAction || got.Confidence != tc.want.Confidence {
				t.Errorf("recovery/confidence: got %+v want %+v", got, tc.want)
			}
			if got.IssueTitle != tc.want.IssueTitle || got.IssueBody != tc.want.IssueBody {
				t.Errorf("issue: got %+v want %+v", got, tc.want)
			}
			if !slices.Equal(got.IssueLabels, tc.want.IssueLabels) {
				t.Errorf("issue_labels: got %+v want %+v", got.IssueLabels, tc.want.IssueLabels)
			}
			if src != tc.wantSource {
				t.Errorf("source: got %s want %s", src, tc.wantSource)
			}
		})
	}
}
