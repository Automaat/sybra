package task

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBoundStoredDocumentLeavesOrdinaryTaskUnchanged(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	original := Task{
		ID: "abc12345", Title: "ordinary", Status: StatusTodo, AgentMode: AgentModeHeadless,
		Body: "## Description\n\nKeep this exactly.\n", CreatedAt: now, UpdatedAt: now,
		AgentRuns: []AgentRun{{AgentID: "a1", Prompt: "short prompt", Result: "short result"}},
	}
	got, err := BoundStoredDocument(original, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("ordinary task changed:\n got: %#v\nwant: %#v", got, original)
	}
}

func TestBoundStoredDocumentBoundsRunHistoryAndLeavesReceipt(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	record := Task{ID: "abc12345", Title: "large", Status: StatusDone, AgentMode: AgentModeHeadless, CreatedAt: now, UpdatedAt: now}
	for i := range 140 {
		record.AgentRuns = append(record.AgentRuns, AgentRun{
			AgentID:   "agent-" + strings.Repeat("x", 20),
			Prompt:    strings.Repeat("prompt🙂", 1000),
			Result:    strings.Repeat("result→", 1000),
			TurnCount: i,
			CostUSD:   1,
		})
	}

	got, err := BoundStoredDocument(record, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := MarshalStored(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) > MaxStoredDocumentBytes {
		t.Fatalf("document is %d bytes, maximum is %d", len(doc), MaxStoredDocumentBytes)
	}
	if len(got.AgentRuns) != MaxStoredAgentRuns {
		t.Fatalf("kept %d runs, want %d", len(got.AgentRuns), MaxStoredAgentRuns)
	}
	if got.AgentRuns[0].TurnCount != 40 || got.AgentRuns[len(got.AgentRuns)-1].TurnCount != 139 {
		t.Fatalf("did not keep newest runs: first=%d last=%d", got.AgentRuns[0].TurnCount, got.AgentRuns[len(got.AgentRuns)-1].TurnCount)
	}
	if got.DocumentCompaction == nil || got.DocumentCompaction.DroppedAgentRuns != 40 || got.DocumentCompaction.TrimmedRunFields != 200 {
		t.Fatalf("compaction receipt = %+v", got.DocumentCompaction)
	}
	if got.DocumentCompaction.DroppedRunCostUSD != 40 {
		t.Fatalf("dropped run cost = %.2f, want 40", got.DocumentCompaction.DroppedRunCostUSD)
	}
	for _, run := range got.AgentRuns {
		if len(run.Prompt) > MaxStoredRunTextBytes || len(run.Result) > MaxStoredRunTextBytes {
			t.Fatalf("run text exceeds per-field bound: prompt=%d result=%d", len(run.Prompt), len(run.Result))
		}
		if !strings.Contains(run.Prompt, "truncated to bound task document") {
			t.Fatalf("trimmed prompt has no visible marker: %q", run.Prompt)
		}
	}
}

func TestBoundStoredDocumentTruncatesBodyOnlyAsLastResort(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	record := Task{
		ID: "abc12345", Title: "huge body", Status: StatusTodo, AgentMode: AgentModeHeadless,
		CreatedAt: now, UpdatedAt: now, Body: strings.Repeat("body🙂", MaxStoredDocumentBytes),
	}
	got, err := BoundStoredDocument(record, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := MarshalStored(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) > MaxStoredDocumentBytes {
		t.Fatalf("document is %d bytes, maximum is %d", len(doc), MaxStoredDocumentBytes)
	}
	if got.DocumentCompaction == nil || !got.DocumentCompaction.BodyTruncated {
		t.Fatalf("missing body-truncation receipt: %+v", got.DocumentCompaction)
	}
	if !strings.Contains(got.Body, "truncated to bound task document") {
		t.Fatalf("body has no visible truncation marker")
	}
}
