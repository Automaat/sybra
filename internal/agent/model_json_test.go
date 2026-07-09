package agent

import (
	"bytes"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/limits"
)

func TestAgentJSONKeySet(t *testing.T) {
	ts := time.Date(2026, 6, 28, 20, 30, 0, 0, time.UTC)

	assertJSONKeys(t, Agent{
		ID:                       "agent-1",
		TaskID:                   "task-1",
		Mode:                     "headless",
		State:                    StateRunning,
		SessionID:                "session-1",
		CostUSD:                  1.25,
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
		ReasoningTokens:          50,
		PremiumRequests:          1.5,
		StartedAt:                ts,
		LastEventAt:              ts.Add(time.Minute),
		LogPath:                  "/tmp/agent.ndjson",
		External:                 true,
		PID:                      1234,
		Command:                  "claude -p",
		Name:                     "implementation",
		Project:                  "Automaat/sybra",
		Provider:                 "claude",
		Model:                    "sonnet",
		ExperimentID:             "experiment-1",
		VariantID:                "variant-1",
		AssignmentUnit:           "task",
		AssignmentKey:            "task-1",
		ReasoningEffort:          "high",
		Prompt:                   "implement",
		TurnCount:                2,
		ToolCalls:                3,
		MaxTurns:                 4,
		PluginErrors:             []string{"plugin failed"},
		EscalationReason:         "needs-human",
		ErrorKind:                "provider",
		ErrorMsg:                 "rate limited",
		AwaitingApproval:         true,
		Resumable:                true,
	}, []string{
		"assignmentKey",
		"assignmentUnit",
		"awaitingApproval",
		"cacheCreationInputTokens",
		"cacheReadInputTokens",
		"command",
		"costUsd",
		"errorKind",
		"errorMsg",
		"escalationReason",
		"experimentId",
		"external",
		"id",
		"inputTokens",
		"lastEventAt",
		"logPath",
		"maxTurns",
		"mode",
		"model",
		"name",
		"outputTokens",
		"pid",
		"pluginErrors",
		"premiumRequests",
		"project",
		"prompt",
		"provider",
		"reasoningEffort",
		"reasoningTokens",
		"resumable",
		"sessionId",
		"startedAt",
		"state",
		"taskId",
		"toolCalls",
		"turnCount",
		"variantId",
	})
}

func TestAgentJSONZeroValueKeySet(t *testing.T) {
	assertJSONKeys(t, Agent{}, []string{
		"costUsd",
		"external",
		"id",
		"lastEventAt",
		"mode",
		"sessionId",
		"startedAt",
		"state",
		"taskId",
	})
}

// TestAgentMarshalJSON_MatchesView locks in that json.Marshal(*Agent) (the
// shape every Wails binding / SSE broker emit / HTTP shim caller produces)
// goes through the mu-guarded View() snapshot rather than reading the live
// struct's fields directly, and that the two serializations agree.
func TestAgentMarshalJSON_MatchesView(t *testing.T) {
	a := &Agent{
		ID:           "agent-1",
		TaskID:       "task-1",
		State:        StateRunning,
		CostUSD:      1.25,
		PluginErrors: []string{"boom"},
	}

	viaPointer, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal *Agent: %v", err)
	}
	viaView, err := json.Marshal(a.View())
	if err != nil {
		t.Fatalf("marshal View: %v", err)
	}
	if !bytes.Equal(viaPointer, viaView) {
		t.Fatalf("json.Marshal(*Agent) != json.Marshal(View)\nagent: %s\nview:  %s", viaPointer, viaView)
	}
}

// TestAgentView_ConcurrentWithMutation exercises View() under -race
// alongside concurrent SetState/AddResultStats/AppendOutput calls — the
// exact shape of the read/write race that motivated the DTO (the runner,
// watchdog, and approval-server goroutines each write while broker.Emit and
// Wails/HTTP handlers read for serialization).
func TestAgentView_ConcurrentWithMutation(t *testing.T) {
	a := &Agent{ID: "agent-1", State: StateRunning}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a.SetState(StateRunning)
			a.AddResultStats("session", float64(i), i, i, i)
			a.AppendOutput(StreamEvent{Type: "assistant"})
		}
	})

	for range 200 {
		_ = a.View()
		_, _ = json.Marshal(a)
	}
	close(stop)
	wg.Wait()
}

func TestStreamEventJSONKeySet(t *testing.T) {
	ts := time.Date(2026, 6, 28, 20, 31, 0, 0, time.UTC)

	assertJSONKeys(t, StreamEvent{
		Type:                     "result",
		Content:                  "done",
		SessionID:                "session-1",
		CostUSD:                  2.5,
		InputTokens:              100,
		OutputTokens:             200,
		CacheCreationInputTokens: 300,
		CacheReadInputTokens:     400,
		ReasoningTokens:          500,
		PremiumRequests:          2.25,
		Subtype:                  "success",
		Timestamp:                ts,
		ErrorType:                "overloaded_error",
		ErrorStatus:              529,
		PlanSteps:                []PlanStep{{Content: "ship", Status: "completed"}},
		PluginErrors:             []string{"plugin failed"},
		ToolCalls:                6,
		LimitSnapshot: &limits.Snapshot{
			Provider:   limits.ProviderClaude,
			Source:     limits.SourceStream,
			Confidence: limits.ConfidenceExact,
			CapturedAt: ts,
		},
	}, []string{
		"cache_creation_input_tokens",
		"cache_read_input_tokens",
		"content",
		"cost_usd",
		"error_status",
		"error_type",
		"input_tokens",
		"limit_snapshot",
		"output_tokens",
		"plan_steps",
		"plugin_errors",
		"premium_requests",
		"reasoning_tokens",
		"session_id",
		"subtype",
		"timestamp",
		"tool_calls",
		"type",
	})
}

func TestStreamEventJSONZeroValueKeySet(t *testing.T) {
	assertJSONKeys(t, StreamEvent{}, []string{
		"timestamp",
		"type",
	})
}

func TestConvoEventJSONKeySet(t *testing.T) {
	ts := time.Date(2026, 6, 28, 20, 32, 0, 0, time.UTC)

	assertJSONKeys(t, ConvoEvent{
		Type:                     "assistant",
		Subtype:                  "message",
		SessionID:                "session-1",
		Text:                     "done",
		ToolUses:                 []ToolUseBlock{{ID: "tool-1", Name: "Read", Input: map[string]any{"file": "x"}}},
		ToolResults:              []ToolResultBlock{{ToolUseID: "tool-1", Content: "ok", IsError: true}},
		CostUSD:                  3.5,
		InputTokens:              11,
		OutputTokens:             22,
		CacheCreationInputTokens: 33,
		CacheReadInputTokens:     44,
		ReasoningTokens:          55,
		PremiumRequests:          3.25,
		LimitSnapshot: &limits.Snapshot{
			Provider:   limits.ProviderCodex,
			Source:     limits.SourceStream,
			Confidence: limits.ConfidenceEstimated,
			CapturedAt: ts,
		},
		IsPartial:   true,
		Timestamp:   ts,
		Raw:         json.RawMessage(`{"raw":true}`),
		ErrorType:   "overloaded_error",
		ErrorStatus: 529,
	}, []string{
		"cacheCreationInputTokens",
		"cacheReadInputTokens",
		"costUsd",
		"errorStatus",
		"errorType",
		"inputTokens",
		"isPartial",
		"limitSnapshot",
		"outputTokens",
		"premiumRequests",
		"raw",
		"reasoningTokens",
		"sessionId",
		"subtype",
		"text",
		"timestamp",
		"toolResults",
		"toolUses",
		"type",
	})
}

func TestConvoEventJSONZeroValueKeySet(t *testing.T) {
	assertJSONKeys(t, ConvoEvent{}, []string{
		"timestamp",
		"type",
	})
}

func assertJSONKeys(t *testing.T, value any, want []string) {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var gotMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &gotMap); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}

	got := make([]string, 0, len(gotMap))
	for k := range gotMap {
		got = append(got, k)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("json keys mismatch\ngot:  %v\nwant: %v\njson: %s", got, want, data)
	}
}
