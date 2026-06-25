package artifact

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func TestPutListRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	content := []byte("# Plan\nDo the thing.")
	meta, err := s.Put("task-abc", Artifact{
		Kind:         KindPlan,
		ProducerRole: "planner",
		StepID:       "plan",
		Content:      content,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if meta.Name != "plan.md" {
		t.Errorf("Name = %q, want plan.md", meta.Name)
	}
	if meta.TaskID != "task-abc" {
		t.Errorf("TaskID = %q, want task-abc", meta.TaskID)
	}

	metas, err := s.List("task-abc")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("List len = %d, want 1", len(metas))
	}

	got, gotMeta, err := s.Read("task-abc", "plan.md")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
	if gotMeta.StepID != "plan" {
		t.Errorf("StepID = %q, want plan", gotMeta.StepID)
	}
}

func TestListIgnoresCorruptIndex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put("task-1", Artifact{Kind: KindPlan, Content: []byte("x")})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	dir := filepath.Join(s.root, "task-1")
	if wErr := os.WriteFile(filepath.Join(dir, "index.json"), []byte("!!!not json!!!"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	metas, err := s.List("task-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("List len = %d, want 1", len(metas))
	}
}

func TestListSkipsMalformedMeta(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put("task-2", Artifact{Kind: KindPlan, Content: []byte("x")})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	dir := filepath.Join(s.root, "task-2")
	if wErr := os.WriteFile(filepath.Join(dir, "bad.meta.json"), []byte("{bad"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	metas, err := s.List("task-2")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("List len = %d, want 1 (bad row skipped)", len(metas))
	}
}

func TestAppend(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	type event struct {
		Step string `json:"step"`
	}
	if err := s.Append("task-3", KindTrace, event{"a"}); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := s.Append("task-3", KindTrace, event{"b"}); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	data, _, err := s.Read("task-3", "trace.jsonl")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	lines := splitLines(data)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	var e0, e1 event
	if err := json.Unmarshal([]byte(lines[0]), &e0); err != nil {
		t.Fatalf("line 0 parse: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &e1); err != nil {
		t.Fatalf("line 1 parse: %v", err)
	}
	if e0.Step != "a" || e1.Step != "b" {
		t.Errorf("order wrong: got %q %q", e0.Step, e1.Step)
	}
}

func splitLines(b []byte) []string {
	var lines []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				lines = append(lines, string(b[start:i]))
			}
			start = i + 1
		}
	}
	return lines
}

func TestRace(t *testing.T) {
	s := newTestStore(t)
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n * 2)

	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, _ = s.Put("race-task", Artifact{
				Kind:    KindGeneric,
				Name:    "generic.md",
				Content: []byte("data"),
			})
			_ = i
		}(i)
		go func() {
			defer wg.Done()
			_ = s.Append("race-task", KindTrace, map[string]any{"t": time.Now().UnixNano()})
		}()
	}
	wg.Wait()

	metas, err := s.List("race-task")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) == 0 {
		t.Error("expected at least one artifact")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put("del-task", Artifact{Kind: KindPlan, Content: []byte("x")})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := s.Delete("del-task"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete("del-task"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	s.mu.Lock()
	_, exists := s.locks["del-task"]
	s.mu.Unlock()
	if exists {
		t.Error("lock entry not pruned after Delete")
	}

	metas, err := s.List("del-task")
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected empty list after delete, got %d", len(metas))
	}
}

func TestReindex(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put("reindex-task", Artifact{Kind: KindPlan, Content: []byte("x")})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	dir := filepath.Join(s.root, "reindex-task")
	if wErr := os.WriteFile(filepath.Join(dir, "index.json"), []byte("corrupt"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}

	if err := s.Reindex("reindex-task"); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("read index.json: %v", err)
	}
	var metas []Meta
	if err := json.Unmarshal(data, &metas); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("index has %d entries, want 1", len(metas))
	}
}

func TestBinaryContent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	content := []byte{0x00, 0x01, 0xFF, 0xFE, 0x0A, 0x00}
	_, err := s.Put("bin-task", Artifact{Kind: KindGeneric, Name: "blob.bin", Content: content})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _, err := s.Read("bin-task", "blob.bin")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %v, want %v", got, content)
	}
}

func TestInvalidTaskID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	cases := []string{"../escape", "foo/bar", "", "task\x00bad"}
	for _, id := range cases {
		if _, err := s.Put(id, Artifact{Kind: KindPlan, Content: []byte("x")}); err == nil {
			t.Errorf("Put(%q) expected error, got nil", id)
		}
		if _, err := s.List(id); err == nil {
			t.Errorf("List(%q) expected error, got nil", id)
		}
		if _, _, err := s.Read(id, "plan.md"); err == nil {
			t.Errorf("Read(%q) expected error, got nil", id)
		}
	}
}

func TestInvalidArtifactName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put("task-valid", Artifact{Kind: KindPlan, Name: "../escape.md", Content: []byte("x")})
	if err == nil {
		t.Error("expected error for hostile name, got nil")
	}
}

func TestListEmptyDir(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	metas, err := s.List("no-such-task")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected empty, got %d", len(metas))
	}
}
