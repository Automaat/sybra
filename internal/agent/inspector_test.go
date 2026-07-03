package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateInspectorVerdict(t *testing.T) {
	tests := []struct {
		name    string
		v       InspectorVerdict
		wantErr bool
	}{
		{"valid stop with rate_limit kind", InspectorVerdict{Recommendation: "stop", ReasonKind: "rate_limit"}, false},
		{"valid stop with generic_stall kind", InspectorVerdict{Recommendation: "stop", ReasonKind: "generic_stall"}, false},
		{"valid stop with reward_hacking kind", InspectorVerdict{Recommendation: "stop", ReasonKind: "reward_hacking"}, false},
		{"valid stop with empty kind (backwards compat)", InspectorVerdict{Recommendation: "stop", ReasonKind: ""}, false},
		{"valid continue", InspectorVerdict{Recommendation: "continue"}, false},
		{"invalid recommendation", InspectorVerdict{Recommendation: "bogus"}, true},
		{"invalid reason_kind", InspectorVerdict{Recommendation: "stop", ReasonKind: "bogus"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInspectorVerdict(&tc.v)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateInspectorVerdict(%+v) err = %v, wantErr %v", tc.v, err, tc.wantErr)
			}
		})
	}
}

func TestParseInspectorOutput(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    InspectorVerdict
		wantErr bool
	}{
		{
			name: "clean stop verdict",
			raw:  `{"result":"{\"stuck\":true,\"reason\":\"loop\",\"recommendation\":\"stop\"}"}`,
			want: InspectorVerdict{Stuck: true, Reason: "loop", Recommendation: "stop"},
		},
		{
			name: "verdict with prose before JSON",
			raw:  `{"result":"Analysis: repeated ls commands.\n{\"stuck\":true,\"reason\":\"repeat\",\"recommendation\":\"stop\"}"}`,
			want: InspectorVerdict{Stuck: true, Reason: "repeat", Recommendation: "stop"},
		},
		{
			name: "continue recommendation",
			raw:  `{"result":"{\"stuck\":false,\"reason\":\"progress\",\"recommendation\":\"continue\"}"}`,
			want: InspectorVerdict{Stuck: false, Reason: "progress", Recommendation: "continue"},
		},
		{
			name: "escalate recommendation",
			raw:  `{"result":"{\"stuck\":true,\"reason\":\"ambiguous\",\"recommendation\":\"escalate\"}"}`,
			want: InspectorVerdict{Stuck: true, Reason: "ambiguous", Recommendation: "escalate"},
		},
		{
			name: "nudge recommendation with steer",
			raw:  `{"result":"{\"stuck\":false,\"reason\":\"retrying failing cmd\",\"recommendation\":\"nudge\",\"nudge\":\"read the error and fix the root cause\"}"}`,
			want: InspectorVerdict{Stuck: false, Reason: "retrying failing cmd", Recommendation: "nudge", Nudge: "read the error and fix the root cause"},
		},
		{
			name:    "invalid recommendation",
			raw:     `{"result":"{\"stuck\":true,\"reason\":\"x\",\"recommendation\":\"kill\"}"}`,
			wantErr: true,
		},
		{
			name:    "empty result",
			raw:     `{"result":""}`,
			wantErr: true,
		},
		{
			name:    "no JSON in result",
			raw:     `{"result":"agent looks fine to me"}`,
			wantErr: true,
		},
		{
			name:    "invalid envelope",
			raw:     `not json`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInspectorOutput([]byte(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (verdict=%+v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExtractLastJSONObject(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{`noise {"a":1} more {"b":2}`, `{"b":2}`},
		{`{"outer":{"inner":1}}`, `{"outer":{"inner":1}}`},
		{`no braces`, ``},
		{`{unbalanced`, ``},
	}
	for _, tc := range tests {
		if got := extractLastJSONObject(tc.in); got != tc.want {
			t.Errorf("extractLastJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestExtractLastJSONObject_BraceInsideString demonstrates that the
// brace-counting walker in extractLastJSONObject does not understand JSON
// string literals — it counts every `{` and `}` byte regardless of whether
// it sits inside a quoted string. A perfectly valid JSON object whose
// string value contains a `{` or `}` character is therefore mis-parsed:
// the walker sees an unbalanced brace and either truncates the result or
// returns an empty string.
//
// This matters because parseInspectorOutput feeds the inspector model's
// `result` text through this function. If the model writes a reason like
// `"saw a stray ) {"`, the verdict is silently lost and the watchdog
// fails to act on a stuck agent.
//
// Fix: parse the result as JSON (or use a real tokenizer that knows about
// string literals) instead of brace counting on raw bytes.
func TestExtractLastJSONObject_BraceInsideString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "open brace inside string value",
			in:   `{"reason": "if (x) {"}`,
			want: `{"reason": "if (x) {"}`,
		},
		{
			name: "close brace inside string value",
			in:   `{"reason": "saw }"}`,
			want: `{"reason": "saw }"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractLastJSONObject(tc.in)
			if got != tc.want {
				t.Errorf("extractLastJSONObject(%q) = %q, want %q (brace inside string literal mis-parsed)", tc.in, got, tc.want)
			}
		})
	}
}

func TestInspectPassesConfiguredClaudeModel(t *testing.T) {
	dir := t.TempDir()
	writeInspectorTestExe(t, filepath.Join(dir, "claude"), `#!/bin/bash
if [[ "$*" != *"--model claude-haiku-4-5-20251001"* ]]; then
  echo "missing configured model flag: $*" >&2
  exit 7
fi
printf '%s\n' '{"result":"{\"stuck\":false,\"reason\":\"progress\",\"recommendation\":\"continue\"}"}'
`)
	t.Setenv("PATH", dir)

	logger := slog.New(slog.DiscardHandler)
	got, err := Inspect(context.Background(), logger, InspectInput{
		AgentID:   "agent-1",
		TaskTitle: "task",
		Model:     "claude-haiku-4-5-20251001",
	})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.Recommendation != "continue" {
		t.Fatalf("Recommendation = %q, want continue", got.Recommendation)
	}
}

func writeInspectorTestExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
