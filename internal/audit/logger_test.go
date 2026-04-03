package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoggerWritesEvent(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	ts := time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC)
	err = l.Log(Event{
		Timestamp: ts,
		Type:      EventTaskCreated,
		TaskID:    "abc123",
		Data:      map[string]any{"title": "test task"},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-04-03.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty file")
	}
}

func TestLoggerRotatesDaily(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	day1 := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)

	_ = l.Log(Event{Timestamp: day1, Type: EventTaskCreated, TaskID: "t1"})
	_ = l.Log(Event{Timestamp: day2, Type: EventTaskCreated, TaskID: "t2"})

	if _, err := os.Stat(filepath.Join(dir, "2026-04-01.ndjson")); err != nil {
		t.Fatal("day1 file missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-04-02.ndjson")); err != nil {
		t.Fatal("day2 file missing")
	}
}
