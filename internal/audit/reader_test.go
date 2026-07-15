package audit

import (
	"testing"
	"time"
)

func TestReadFiltersEvents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 4, 3, 10, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: ts, Type: EventTaskCreated, TaskID: "t1"},
		{Timestamp: ts.Add(time.Minute), Type: EventAgentStarted, TaskID: "t1", AgentID: "a1"},
		{Timestamp: ts.Add(2 * time.Minute), Type: EventTaskCreated, TaskID: "t2"},
	}
	for _, e := range events {
		if err := l.Log(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close()

	tests := []struct {
		name  string
		query Query
		want  int
	}{
		{"all", Query{Since: ts.Add(-time.Hour), Until: ts.Add(time.Hour)}, 3},
		{"by type prefix", Query{Since: ts.Add(-time.Hour), Until: ts.Add(time.Hour), Type: "task"}, 2},
		{"by task id", Query{Since: ts.Add(-time.Hour), Until: ts.Add(time.Hour), TaskID: "t1"}, 2},
		{"by exact type", Query{Since: ts.Add(-time.Hour), Until: ts.Add(time.Hour), Type: EventAgentStarted}, 1},
		{"time window", Query{Since: ts.Add(90 * time.Second), Until: ts.Add(time.Hour)}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Read(dir, tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Errorf("got %d events, want %d", len(got), tt.want)
			}
		})
	}
}

func TestReadEmptyDir(t *testing.T) {
	t.Parallel()
	events, err := Read(t.TempDir(), Query{
		Since: time.Now().Add(-time.Hour),
		Until: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}
