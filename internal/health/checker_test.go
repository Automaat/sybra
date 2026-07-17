package health

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
)

func TestOwnedProcessesOwnsSeparatesPIDsAndGroups(t *testing.T) {
	t.Parallel()

	owned := OwnedProcesses{
		PIDs:          map[int]bool{100: true},
		ProcessGroups: map[int]bool{200: true},
	}

	tests := []struct {
		name string
		pid  int
		pgid int
		want bool
	}{
		{"exact pid", 100, 999, true},
		{"trusted process group", 101, 200, true},
		{"pid set is not a process group", 101, 100, false},
		{"unknown", 101, 999, false},
		{"zero values", 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := owned.Owns(tt.pid, tt.pgid); got != tt.want {
				t.Fatalf("Owns(%d, %d) = %v, want %v", tt.pid, tt.pgid, got, tt.want)
			}
		})
	}
}

func TestCheckerIncludesDockerReclaimableFinding(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}

	c := New(t.TempDir(), task.NewManager(store, nil), home, slog.New(slog.DiscardHandler), nil, nil)
	c.docker = func(context.Context) ([]byte, error) {
		return []byte(`{"Size":"25GiB","Reclaimable":"20GiB (80%)"}`), nil
	}

	c.check(t.Context())

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}
	if report.Docker == nil {
		t.Fatal("Docker = nil, want sample")
	}
	if report.Docker.ReclaimableBytes != dockerReclaimableThresholdBytes {
		t.Fatalf("Docker.ReclaimableBytes = %d, want %d", report.Docker.ReclaimableBytes, dockerReclaimableThresholdBytes)
	}

	var dockerFinding *Finding
	for i := range report.Findings {
		if report.Findings[i].Category == CatDockerReclaimable {
			dockerFinding = &report.Findings[i]
			break
		}
	}
	if dockerFinding == nil {
		t.Fatalf("expected %q finding, got %v", CatDockerReclaimable, findingCategories(report.Findings))
	}
	if dockerFinding.Fingerprint != string(CatDockerReclaimable) {
		t.Fatalf("Fingerprint = %q, want %q", dockerFinding.Fingerprint, CatDockerReclaimable)
	}
}

func TestCheckerSkipsSandboxCheckWhenUnwired(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}

	c := New(t.TempDir(), task.NewManager(store, nil), home, slog.New(slog.DiscardHandler), nil, nil)
	c.check(t.Context())

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}
	for _, f := range report.Findings {
		if f.Category == CatSandboxCleanup {
			t.Fatalf("unexpected %q finding with no sandboxQuarantine wired: %+v", CatSandboxCleanup, f)
		}
	}
}

func TestCheckerIncludesSandboxCleanupFinding(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}

	c := New(t.TempDir(), task.NewManager(store, nil), home, slog.New(slog.DiscardHandler), nil, nil)
	c.SetSandboxQuarantine(func() []sandbox.QuarantineEntry {
		return []sandbox.QuarantineEntry{
			{TaskID: "task-quarantined", Path: "/data/sandboxes/task-quarantined", BytesRetained: 2048, Attempts: 4, LastError: "permission denied"},
		}
	})

	c.check(t.Context())

	report := c.LatestReport()
	if report == nil {
		t.Fatal("LatestReport returned nil")
	}
	var found *Finding
	for i := range report.Findings {
		if report.Findings[i].Category == CatSandboxCleanup {
			found = &report.Findings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected %q finding, got %v", CatSandboxCleanup, findingCategories(report.Findings))
	}
	if found.TaskID != "task-quarantined" {
		t.Errorf("TaskID = %q, want task-quarantined", found.TaskID)
	}
	if found.Fingerprint != string(CatSandboxCleanup)+":task-quarantined" {
		t.Errorf("Fingerprint = %q, want %q", found.Fingerprint, string(CatSandboxCleanup)+":task-quarantined")
	}
	if report.Score != ScoreCritical {
		t.Errorf("Score = %q, want critical", report.Score)
	}
}
