package bgop

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) *Tracker {
	t.Helper()
	diskPath := filepath.Join(t.TempDir(), "bgops.json")
	return NewTracker(func(string, any) {}, diskPath, nil)
}

func TestTracker_StartCompleteFail(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeClone, "cloning repo", "proj1", "task1")
	if id == "" {
		t.Fatal("expected non-empty operation id")
	}

	ops := tr.List()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op after Start, got %d", len(ops))
	}
	if ops[0].Status != StatusRunning {
		t.Errorf("expected status running, got %s", ops[0].Status)
	}

	tr.UpdatePhase(id, "fetching")
	ops = tr.List()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op after UpdatePhase, got %d", len(ops))
	}
	if ops[0].Phase != "fetching" {
		t.Errorf("expected phase 'fetching', got %q", ops[0].Phase)
	}

	tr.Complete(id)
	ops = tr.List()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op after Complete, got %d", len(ops))
	}
	if ops[0].Status != StatusDone {
		t.Errorf("expected status done, got %s", ops[0].Status)
	}
	if ops[0].Phase != "" {
		t.Errorf("expected phase cleared on completion, got %q", ops[0].Phase)
	}
	if ops[0].CompletedAt.IsZero() {
		t.Error("expected CompletedAt to be set")
	}
}

func TestTracker_Fail(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeWorktreePrep, "prep", "proj1", "task1")
	tr.Fail(id, errors.New("boom"))

	ops := tr.List()
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	if ops[0].Status != StatusFailed {
		t.Errorf("expected status failed, got %s", ops[0].Status)
	}
	if ops[0].Error != "boom" {
		t.Errorf("expected error 'boom', got %q", ops[0].Error)
	}
}

func TestTracker_UnknownID_NoOp(t *testing.T) {
	tr := newTestTracker(t)

	// None of these should panic or create entries for an unknown id.
	tr.UpdatePhase("missing", "phase")
	tr.Complete("missing")
	tr.Fail("missing", errors.New("boom"))

	if ops := tr.List(); len(ops) != 0 {
		t.Fatalf("expected 0 ops, got %d", len(ops))
	}
}

func TestTracker_List_FiltersExpiredCompletions(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeClone, "cloning", "proj1", "task1")
	tr.Complete(id)

	// Manually age the completion past the TTL.
	tr.mu.Lock()
	op := tr.ops[id]
	if op == nil {
		tr.mu.Unlock()
		t.Fatalf("expected op %q to exist", id)
	}
	op.CompletedAt = time.Now().Add(-completionTTL - time.Minute)
	tr.mu.Unlock()

	if ops := tr.List(); len(ops) != 0 {
		t.Fatalf("expected expired completed op to be filtered out, got %d", len(ops))
	}
}

func TestTracker_List_KeepsRunningRegardlessOfAge(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeClone, "cloning", "proj1", "task1")
	tr.mu.Lock()
	op := tr.ops[id]
	if op == nil {
		tr.mu.Unlock()
		t.Fatalf("expected op %q to exist", id)
	}
	op.StartedAt = time.Now().Add(-completionTTL - time.Hour)
	tr.mu.Unlock()

	ops := tr.List()
	if len(ops) != 1 {
		t.Fatalf("expected running op to survive regardless of age, got %d", len(ops))
	}
}

func TestTracker_SaveAndLoadFromDisk_RoundTrip(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeClone, "cloning", "proj1", "task1")
	tr.Complete(id)

	if _, err := os.Stat(tr.diskPath); err != nil {
		t.Fatalf("expected persisted file at %s: %v", tr.diskPath, err)
	}

	// Fresh tracker sharing the same disk path should restore the op.
	tr2 := NewTracker(func(string, any) {}, tr.diskPath, nil)
	tr2.LoadFromDisk()

	ops := tr2.List()
	if len(ops) != 1 {
		t.Fatalf("expected 1 restored op, got %d", len(ops))
	}
	if ops[0].ID != id {
		t.Errorf("expected restored op id %s, got %s", id, ops[0].ID)
	}
	if ops[0].Status != StatusDone {
		t.Errorf("expected restored status done, got %s", ops[0].Status)
	}
	if ops[0].Phase != "" {
		t.Errorf("expected restored phase to remain non-persistent, got %q", ops[0].Phase)
	}
}

func TestTracker_LoadFromDisk_MarksRunningOpsFailed(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "bgops.json")
	ops := []Operation{
		{
			ID:        "op1",
			Type:      TypeClone,
			Label:     "cloning",
			Status:    StatusRunning,
			StartedAt: time.Now().UTC(),
		},
	}
	data, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(diskPath, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tr := NewTracker(func(string, any) {}, diskPath, nil)
	tr.LoadFromDisk()

	loaded := tr.List()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 restored op, got %d", len(loaded))
	}
	if loaded[0].Status != StatusFailed {
		t.Errorf("expected running op to be marked failed after restart, got %s", loaded[0].Status)
	}
	if loaded[0].Error == "" {
		t.Error("expected an error message explaining the interruption")
	}
	if loaded[0].CompletedAt.IsZero() {
		t.Error("expected CompletedAt to be set for the reclassified op")
	}

	data, err = os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var persisted []Operation
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal persisted file: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected 1 persisted op after rewrite, got %d", len(persisted))
	}
	if persisted[0].Status != StatusFailed {
		t.Errorf("expected rewritten op status failed, got %s", persisted[0].Status)
	}
	if persisted[0].Phase != "" {
		t.Errorf("expected rewritten op phase to be cleared, got %q", persisted[0].Phase)
	}
}

func TestTracker_LoadFromDisk_DiscardsExpiredCompletions(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "bgops.json")
	ops := []Operation{
		{
			ID:          "old-done",
			Type:        TypeClone,
			Status:      StatusDone,
			StartedAt:   time.Now().Add(-2 * completionTTL),
			CompletedAt: time.Now().Add(-2 * completionTTL),
		},
		{
			ID:          "recent-done",
			Type:        TypeClone,
			Status:      StatusDone,
			StartedAt:   time.Now().Add(-time.Minute),
			CompletedAt: time.Now().Add(-time.Minute),
		},
	}
	data, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(diskPath, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tr := NewTracker(func(string, any) {}, diskPath, nil)
	tr.LoadFromDisk()

	loaded := tr.List()
	if len(loaded) != 1 {
		t.Fatalf("expected only the recent completion to survive, got %d", len(loaded))
	}
	if loaded[0].ID != "recent-done" {
		t.Errorf("expected recent-done to survive, got %s", loaded[0].ID)
	}

	data, err = os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	var persisted []Operation
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal rewritten file: %v", err)
	}
	if len(persisted) != 1 || persisted[0].ID != "recent-done" {
		t.Fatalf("expected rewritten file to keep only recent-done, got %+v", persisted)
	}
}

func TestTracker_SaveToDisk_DropsTransientState(t *testing.T) {
	tr := newTestTracker(t)

	id := tr.Start(TypeClone, "cloning", "proj1", "task1")
	tr.UpdatePhase(id, "fetching")

	tr.mu.Lock()
	op := tr.ops[id]
	if op == nil {
		tr.mu.Unlock()
		t.Fatalf("expected op %q to exist", id)
	}
	op.Status = StatusDone
	op.CompletedAt = time.Now().Add(-completionTTL - time.Minute)
	tr.mu.Unlock()

	doneID := tr.Start(TypeWorktreePrep, "prep", "proj1", "task1")
	tr.UpdatePhase(doneID, "phase should not persist")
	tr.Complete(doneID)

	data, err := os.ReadFile(tr.diskPath)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var persisted []Operation
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal persisted file: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected only the non-expired op to persist, got %d", len(persisted))
	}
	if persisted[0].ID != doneID {
		t.Fatalf("expected persisted op %q, got %q", doneID, persisted[0].ID)
	}
	if persisted[0].Phase != "" {
		t.Errorf("expected phase to stay non-persistent, got %q", persisted[0].Phase)
	}
	if _, ok := tr.ops[id]; ok {
		t.Fatalf("expected expired op %q to be pruned from memory during save", id)
	}
}

func TestTracker_LoadFromDisk_MissingFile_NoErrorLogged(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "does-not-exist.json")
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	tr := NewTracker(func(string, any) {}, diskPath, logger)
	tr.LoadFromDisk()

	if ops := tr.List(); len(ops) != 0 {
		t.Fatalf("expected 0 ops on missing file, got %d", len(ops))
	}
	if logBuf.Len() != 0 {
		t.Errorf("expected no log output for a missing file on first startup, got %q", logBuf.String())
	}
}

func TestTracker_LoadFromDisk_CorruptedJSON_LogsAndKeepsRunning(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "bgops.json")
	if err := os.WriteFile(diskPath, []byte("{ not valid json "), 0o644); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	tr := NewTracker(func(string, any) {}, diskPath, logger)

	// Should not panic on corrupted data.
	tr.LoadFromDisk()

	if ops := tr.List(); len(ops) != 0 {
		t.Fatalf("expected 0 ops after failing to parse corrupted disk state, got %d", len(ops))
	}
	if logBuf.Len() == 0 {
		t.Error("expected corrupted JSON to be logged")
	}

	// Tracker must remain fully usable after a failed load.
	id := tr.Start(TypeClone, "cloning", "proj1", "task1")
	if ops := tr.List(); len(ops) != 1 || ops[0].ID != id {
		t.Fatalf("expected tracker to remain usable after corrupted load, got %+v", ops)
	}
}

func TestTracker_SaveToDisk_WriteFailureIsLogged(t *testing.T) {
	// Point diskPath at a location whose parent directory doesn't exist so
	// the atomic write's temp-file creation fails.
	diskPath := filepath.Join(t.TempDir(), "missing-dir", "bgops.json")
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	tr := NewTracker(func(string, any) {}, diskPath, logger)
	tr.Start(TypeClone, "cloning", "proj1", "task1")

	if logBuf.Len() == 0 {
		t.Error("expected a write failure to be logged")
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Errorf("expected no file to be written, got err=%v", err)
	}
}
