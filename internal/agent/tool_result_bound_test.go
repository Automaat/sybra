package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
)

func TestBindToolResultEvent_PersistsArtifactAndKeepsDiagnostic(t *testing.T) {
	store := artifact.New(t.TempDir())
	raw := strings.Repeat("ok   github.com/Automaat/sybra/internal/pkg 0.01s\n", 80) +
		"internal/sybra/app.go:217:13: undefined: missingDependency\n" +
		"internal/sybra/app.go:217:13: compile failed after refactor\n" +
		strings.Repeat("lint note: checked another file successfully\n", 80)
	ev := StreamEvent{
		Type: "tool_result",
		toolResults: []ToolResultBlock{{
			ToolUseID: "toolu_large",
			Content:   raw,
			IsError:   true,
		}},
	}

	got := bindToolResultEvent("task-bound", string(RoleImplementation), store, ev)

	if !strings.Contains(got.Content, "[tool output truncated]") {
		t.Fatalf("summary missing truncation marker:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "artifact_name: tool-output-toolu_large-") {
		t.Fatalf("summary missing artifact name:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "undefined: missingDependency") {
		t.Fatalf("summary lost diagnostic:\n%s", got.Content)
	}
	if len(got.Content) >= len(raw) {
		t.Fatalf("summary len = %d, want smaller than raw %d", len(got.Content), len(raw))
	}
	if got.toolResults[0].Content != got.Content {
		t.Fatalf("toolResults content mismatch:\ncontent=%q\nblock=%q", got.Content, got.toolResults[0].Content)
	}

	metas, err := store.List("task-bound")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(metas))
	}
	if !strings.HasPrefix(metas[0].Name, "tool-output-toolu_large-") {
		t.Fatalf("artifact name = %q, want tool-output-toolu_large-*", metas[0].Name)
	}
	if metas[0].ProducerRole != string(RoleImplementation) {
		t.Fatalf("producer role = %q, want %q", metas[0].ProducerRole, RoleImplementation)
	}
	data, _, err := store.Read("task-bound", metas[0].Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != raw {
		t.Fatalf("artifact content mismatch")
	}
}

func TestBindToolResultEvent_FallsBackToHeadAndTail(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 220; i++ {
		b.WriteString("plain line ")
		b.WriteString(strings.Repeat("x", 20))
		b.WriteString("\n")
	}
	raw := b.String()
	ev := StreamEvent{
		Type: "tool_result",
		toolResults: []ToolResultBlock{{
			ToolUseID: "toolu_plain",
			Content:   raw,
		}},
	}

	got := bindToolResultEvent("task-plain", "", nil, ev)

	if !strings.Contains(got.Content, "head lines 1-12:") {
		t.Fatalf("summary missing head excerpt:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "tail lines 209-220:") {
		t.Fatalf("summary missing tail excerpt:\n%s", got.Content)
	}
}

func TestParseLogFileWithArtifacts_BoundsReplayedToolResults(t *testing.T) {
	store := artifact.New(t.TempDir())
	raw := strings.Repeat("ok   pkg\n", 180) +
		"internal/sybra/app.go:217:13: undefined: replayedMissingSymbol\n" +
		strings.Repeat("postlude\n", 180)
	logPath := filepath.Join(t.TempDir(), "agent.ndjson")
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_replay","content":` +
		strconv.Quote(raw) + `,"is_error":true}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseLogFileWithArtifacts(logPath, 0, "claude", "task-replay", "", store)
	if err != nil {
		t.Fatalf("ParseLogFileWithArtifacts: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Content, "artifact_name: tool-output-toolu_replay-") {
		t.Fatalf("replayed content missing artifact reference:\n%s", events[0].Content)
	}
	if !strings.Contains(events[0].Content, "replayedMissingSymbol") {
		t.Fatalf("replayed content lost diagnostic:\n%s", events[0].Content)
	}
	metas, err := store.List("task-replay")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(metas))
	}
}

func TestParseLogFileWithArtifacts_BoundsOversizedLogLine(t *testing.T) {
	store := artifact.New(t.TempDir())
	raw := strings.Repeat("ok   pkg\n", 70000) +
		"internal/sybra/app.go:217:13: undefined: oversizedReplaySymbol\n" +
		strings.Repeat("postlude\n", 70000)
	logPath := filepath.Join(t.TempDir(), "agent.ndjson")
	line := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_huge","content":` +
		strconv.Quote(raw) + `,"is_error":true}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ParseLogFileWithArtifacts(logPath, 0, "claude", "task-huge", "", store)
	if err != nil {
		t.Fatalf("ParseLogFileWithArtifacts: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if !strings.Contains(events[0].Content, "artifact_name: tool-output-toolu_huge-") {
		t.Fatalf("oversized replay missing artifact reference:\n%s", events[0].Content)
	}
	if !strings.Contains(events[0].Content, "oversizedReplaySymbol") {
		t.Fatalf("oversized replay lost diagnostic:\n%s", events[0].Content)
	}
	metas, err := store.List("task-huge")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(metas))
	}
}
