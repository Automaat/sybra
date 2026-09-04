package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/task"
)

func createTaskForProgress(t *testing.T, title string) string {
	t.Helper()
	code, out := runCLI(t, "--json", "create", "--title", title)
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, out)
	}
	var created task.Task
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("unmarshal created: %v (%s)", err, out)
	}
	return created.ID
}

func TestProgressAddAndList(t *testing.T) {
	setupStore(t)
	id := createTaskForProgress(t, "progress cli task")

	code, out := runCLI(t, "--json", "progress", "add", id, "--kind", "decision", "--message", "chose headless")
	if code != 0 {
		t.Fatalf("progress add exit %d: %s", code, out)
	}
	var added artifact.ProgressEntry
	if err := json.Unmarshal([]byte(out), &added); err != nil {
		t.Fatalf("unmarshal added: %v (%s)", err, out)
	}
	if added.Kind != "decision" || added.Message != "chose headless" || added.Ts.IsZero() {
		t.Fatalf("added = %+v", added)
	}

	code, out = runCLI(t, "--json", "progress", "list", id)
	if code != 0 {
		t.Fatalf("progress list exit %d: %s", code, out)
	}
	var entries []artifact.ProgressEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal list: %v (%s)", err, out)
	}
	if len(entries) != 1 || entries[0].Message != "chose headless" {
		t.Fatalf("list = %+v", entries)
	}

	code, out = runCLI(t, "--json", "get", id)
	if code != 0 {
		t.Fatalf("get exit %d: %s", code, out)
	}
	var got task.Task
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal get: %v (%s)", err, out)
	}
	if strings.Contains(got.Body, "chose headless") {
		t.Fatalf("progress leaked into task body: %q", got.Body)
	}
}

func TestProgressAddRejectsInvalidKind(t *testing.T) {
	setupStore(t)
	id := createTaskForProgress(t, "bad kind task")
	code, _ := runCLI(t, "--json", "progress", "add", id, "--kind", "bogus", "--message", "x")
	if code == 0 {
		t.Fatal("progress add with invalid kind exit 0, want non-zero")
	}
}

func TestProgressAddRequiresMessage(t *testing.T) {
	setupStore(t)
	id := createTaskForProgress(t, "no message task")
	code, _ := runCLI(t, "--json", "progress", "add", id)
	if code == 0 {
		t.Fatal("progress add without --message exit 0, want non-zero")
	}
}

func TestUpdateWritesDecisionProgressForHumanRequiredUnblock(t *testing.T) {
	setupStore(t)
	id := createTaskForProgress(t, "manual unblock")

	code, out := runCLI(t, "--json", "update", id, "--status", "human-required")
	if code != 0 {
		t.Fatalf("set human-required exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "update", id,
		"--status", "todo",
		"--status-reason", "operator approved retry after clearing stale watchdog counters",
	)
	if code != 0 {
		t.Fatalf("update exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "progress", "list", id)
	if code != 0 {
		t.Fatalf("progress list exit %d: %s", code, out)
	}
	var entries []artifact.ProgressEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal list: %v (%s)", err, out)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one decision entry", entries)
	}
	if entries[0].Kind != artifact.ProgressKindDecision {
		t.Fatalf("kind = %q, want %q", entries[0].Kind, artifact.ProgressKindDecision)
	}
	if !strings.Contains(entries[0].Message, "human-required to todo") {
		t.Fatalf("message = %q, want transition detail", entries[0].Message)
	}
	if !strings.Contains(entries[0].Message, "operator approved retry after clearing stale watchdog counters") {
		t.Fatalf("message = %q, want operator reason", entries[0].Message)
	}
}

func TestReopenWritesDecisionProgressForHumanRequiredTask(t *testing.T) {
	setupStore(t)
	id := createTaskForProgress(t, "reopen unblock")

	code, out := runCLI(t, "--json", "update", id, "--status", "human-required")
	if code != 0 {
		t.Fatalf("set human-required exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "reopen", id)
	if code != 0 {
		t.Fatalf("reopen exit %d: %s", code, out)
	}

	code, out = runCLI(t, "--json", "progress", "list", id)
	if code != 0 {
		t.Fatalf("progress list exit %d: %s", code, out)
	}
	var entries []artifact.ProgressEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal list: %v (%s)", err, out)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want one decision entry", entries)
	}
	if entries[0].Kind != artifact.ProgressKindDecision {
		t.Fatalf("kind = %q, want %q", entries[0].Kind, artifact.ProgressKindDecision)
	}
	if !strings.Contains(entries[0].Message, "human-required to todo") {
		t.Fatalf("message = %q, want transition detail", entries[0].Message)
	}
}
