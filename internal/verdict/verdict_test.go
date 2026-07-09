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
			input:      `{"decision":"human","summary":"needs scope clarification"}`,
			want:       Decision{Decision: "human", Summary: "needs scope clarification"},
			wantSource: SourceJSON,
		},
		{
			name: "bare json sybra_bug",
			input: `{"decision":"sybra_bug","summary":"verify_commits flipped despite push",` +
				`"issue_title":"fix(workflow): verify_commits race","issue_body":"## What\nrace","issue_labels":["workflow"]}`,
			want: Decision{
				Decision: "sybra_bug", Summary: "verify_commits flipped despite push",
				IssueTitle: "fix(workflow): verify_commits race", IssueBody: "## What\nrace",
				IssueLabels: []string{"workflow"},
			},
			wantSource: SourceJSON,
		},
		{
			name:       "bare json with trailing prose",
			input:      `{"decision":"human","summary":"ok"}` + "\n\nThanks for checking!",
			want:       Decision{Decision: "human", Summary: "ok"},
			wantSource: SourceJSON,
		},
		{
			name: "fenced fallback when no bare json",
			input: "Looks like a real ambiguity.\n\n```sybra-verdict\n" +
				`{"decision":"human","summary":"needs scope clarification"}` +
				"\n```\n",
			want:       Decision{Decision: "human", Summary: "needs scope clarification"},
			wantSource: SourceFence,
		},
		{
			name: "bare json precedence over fenced block",
			input: `{"decision":"human","summary":"bare wins"}` +
				"\n\n```sybra-verdict\n" +
				`{"decision":"sybra_bug","summary":"fenced loses"}` +
				"\n```\n",
			want:       Decision{Decision: "human", Summary: "bare wins"},
			wantSource: SourceJSON,
		},
		{
			name:       "case-mismatched decision normalizes",
			input:      `{"decision":"HUMAN","summary":"still valid"}`,
			want:       Decision{Decision: "human", Summary: "still valid"},
			wantSource: SourceJSON,
		},
		{
			name:    "invalid decision value",
			input:   `{"decision":"maybe","summary":"x"}`,
			wantErr: true,
		},
		{
			name:    "empty summary",
			input:   `{"decision":"human","summary":""}`,
			wantErr: true,
		},
		{
			name:    "whitespace summary",
			input:   `{"decision":"human","summary":"   "}`,
			wantErr: true,
		},
		{
			name:    "issue_labels wrong type",
			input:   `{"decision":"sybra_bug","summary":"x","issue_labels":[1,2,3]}`,
			wantErr: true,
		},
		{
			name:  "issue_labels trimmed and blanks dropped",
			input: `{"decision":"sybra_bug","summary":"x","issue_labels":[" workflow ","",  "  ","bug"]}`,
			want: Decision{
				Decision: "sybra_bug", Summary: "x",
				IssueLabels: []string{"workflow", "bug"},
			},
			wantSource: SourceJSON,
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
			input:   `{"decision":"sybra_bug","summary":"test","issue_title":"test title","issue_body":"test body"}`,
			wantErr: true,
		},
		{
			name:    "placeholder summary case-insensitive fails closed",
			input:   `{"decision":"human","summary":"  Test  "}`,
			wantErr: true,
		},
		{
			name: "placeholder issue title fails closed even with real summary",
			input: `{"decision":"sybra_bug","summary":"verify_commits flipped despite push",` +
				`"issue_title":"test title","issue_body":"real body"}`,
			wantErr: true,
		},
		{
			name: "summary merely mentioning test is not a placeholder",
			input: `{"decision":"sybra_bug","summary":"the test suite is flaky",` +
				`"issue_title":"fix(ci): flaky test","issue_body":"real body"}`,
			want: Decision{
				Decision: "sybra_bug", Summary: "the test suite is flaky",
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
			input:   `{"decision":"sybra_bug","summary":"test","issue_title":"test title","issue_body":"test body"}`,
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
			if got.Decision != tc.want.Decision || got.Summary != tc.want.Summary {
				t.Errorf("decision/summary: got %+v want %+v", got, tc.want)
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
