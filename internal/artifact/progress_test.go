package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendProgressRoundTrip(t *testing.T) {
	s := newTestStore(t)
	const taskID = "task-progress-1"

	entries := []ProgressEntry{
		{Kind: ProgressKindProgress, Role: "implementation", Message: "started work"},
		{Kind: ProgressKindDecision, Message: "chose headless mode"},
		{Kind: ProgressKindFailure, Message: "build broke"},
	}
	for _, e := range entries {
		if err := s.AppendProgress(taskID, e); err != nil {
			t.Fatalf("AppendProgress: %v", err)
		}
	}

	got, err := s.ReadProgress(taskID)
	if err != nil {
		t.Fatalf("ReadProgress: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("want %d entries, got %d", len(entries), len(got))
	}
	want := []ProgressEntry{entries[2], entries[1], entries[0]}
	for i, e := range want {
		if got[i].Kind != e.Kind || got[i].Message != e.Message {
			t.Errorf("entry %d = {%s %q}, want newest-first {%s %q}", i, got[i].Kind, got[i].Message, e.Kind, e.Message)
		}
		if got[i].Ts.IsZero() {
			t.Errorf("entry %d has zero timestamp; AppendProgress should stamp it", i)
		}
	}
}

func TestAppendProgressRejectsInvalidKind(t *testing.T) {
	s := newTestStore(t)
	err := s.AppendProgress("task-x", ProgressEntry{Kind: "bogus", Message: "hi"})
	if err == nil {
		t.Fatal("want error for invalid kind, got nil")
	}
}

func TestReadProgressMissingIsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ReadProgress("task-none")
	if err != nil {
		t.Fatalf("ReadProgress on missing: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestReadProgressSkipsMalformedLine(t *testing.T) {
	s := newTestStore(t)
	const taskID = "task-malformed"
	if err := s.AppendProgress(taskID, ProgressEntry{Kind: ProgressKindProgress, Message: "good"}); err != nil {
		t.Fatalf("AppendProgress: %v", err)
	}

	dir, err := s.taskDir(taskID)
	if err != nil {
		t.Fatalf("taskDir: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, KindProgress.defaultName()), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = f.Close()

	got, err := s.ReadProgress(taskID)
	if err != nil {
		t.Fatalf("ReadProgress: %v", err)
	}
	if len(got) != 1 || got[0].Message != "good" {
		t.Fatalf("want 1 valid entry, got %+v", got)
	}
}

func TestValidProgressKind(t *testing.T) {
	for _, k := range ProgressKinds() {
		if !ValidProgressKind(k) {
			t.Errorf("ProgressKinds() member %q rejected by ValidProgressKind", k)
		}
	}
	if ValidProgressKind("nope") {
		t.Error("ValidProgressKind accepted an unknown kind")
	}
}
