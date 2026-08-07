package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestInspectorVerdictSchemaIsCodexStrict asserts inspectorVerdictSchema is a
// root object schema with additionalProperties:false and every struct field
// (including the omitempty ones, nudge and reason_kind) listed as required —
// codex's strict output-schema mode rejects a schema that omits any property
// from "required".
func TestInspectorVerdictSchemaIsCodexStrict(t *testing.T) {
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal([]byte(inspectorVerdictSchema), &schema); err != nil {
		t.Fatalf("unmarshal inspectorVerdictSchema: %v", err)
		panic("unreachable")
	}
	if schema.Type != "object" {
		t.Fatalf("type = %q, want object", schema.Type)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("additionalProperties = %v, want false", schema.AdditionalProperties)
		panic("unreachable")
	}
	required := make(map[string]bool, len(schema.Required))
	for _, f := range schema.Required {
		required[f] = true
	}
	for field := range schema.Properties {
		if !required[field] {
			t.Fatalf("codex strict output schema requires every property; %q missing from required", field)
		}
	}
	for _, want := range []string{"stuck", "reason", "recommendation", "nudge", "reason_kind"} {
		if !required[want] {
			t.Fatalf("required is missing %q: %v", want, schema.Required)
		}
	}
}

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
		{"valid stop with empty nudge and reason_kind (codex strict emission)", InspectorVerdict{Recommendation: "stop", Nudge: "", ReasonKind: ""}, false},
		{"invalid recommendation", InspectorVerdict{Recommendation: "bogus"}, true},
		{"invalid reason_kind", InspectorVerdict{Recommendation: "stop", ReasonKind: "bogus"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInspectorVerdict(&tc.v)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateInspectorVerdict(%+v) err = %v, wantErr %v", tc.v, err, tc.wantErr)
				panic("unreachable")
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
					panic("unreachable")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
				panic("unreachable")
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The inspector model wraps its verdict in prose, so this call site depends on
// the shared scanner's two hard cases: a brace inside a string value (the model
// quotes code in its reason) and a decoy object before the want answer.
func TestParseInspectorOutputResolvesTheRealObject(t *testing.T) {
	want := `{"recommendation":"continue","reason":"looks fine"}`
	tests := []struct {
		name string
		raw  string
	}{
		{name: "prose around", raw: "My read:\n" + want + "\ndone."},
		{name: "fenced", raw: "```json\n" + want + "\n```"},
		{name: "open brace inside a string value", raw: `{"recommendation":"stop","reason":"if (x) {"}` + "\nrevised:\n" + want},
		{name: "close brace inside a string value", raw: `{"recommendation":"stop","reason":"saw }"}` + "\nrevised:\n" + want},
		{name: "decoy object first", raw: `{"recommendation":"stop","reason":"draft"}` + "\nOn reflection:\n" + want},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInspectorOutput([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseInspectorOutput: %v", err)
				panic("unreachable")
			}
			if got.Recommendation != "continue" || got.Reason != "looks fine" {
				t.Errorf("resolved the wrong object: %+v", got)
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
		panic("unreachable")
	}
	if got.Recommendation != "continue" {
		t.Fatalf("Recommendation = %q, want continue", got.Recommendation)
	}
}

func writeInspectorTestExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
		panic("unreachable")
	}
}
