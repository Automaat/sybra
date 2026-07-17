package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// captureStderr runs fn with os.Stderr redirected. Every rejection below must
// be identifiable by its message: rc alone proves nothing here, since a guard
// that failed to fire would still exit 1 when the gh call errored out.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	return <-done
}

func TestCmdPR_GuardsBeforeReachingGH(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		withPR  int
		wantRC  int
		wantMsg string
		explain string
	}{
		{
			name:    "no subcommand",
			args:    nil,
			wantRC:  1,
			wantMsg: "usage: pr create",
			explain: "bare `pr` must print usage, not panic",
		},
		{
			name:    "unknown subcommand",
			args:    []string{"open"},
			wantRC:  1,
			wantMsg: "unknown pr subcommand",
			explain: "only create exists today",
		},
		{
			name:    "missing task id",
			args:    []string{"create", "--repo", "o/r", "--head", "b"},
			wantRC:  1,
			wantMsg: "usage: pr create",
			explain: "the task id is what the PR number gets recorded against",
		},
		{
			name:    "missing repo",
			args:    []string{"create", "TASKID", "--head", "b"},
			wantRC:  1,
			wantMsg: "--repo and --head are required",
			explain: "gh pr create cannot infer the base repo in a bare Job clone",
		},
		{
			name:    "missing head",
			args:    []string{"create", "TASKID", "--repo", "o/r"},
			wantRC:  1,
			wantMsg: "--repo and --head are required",
			explain: "without --head gh would open a PR from whatever is checked out",
		},
		{
			name:    "unknown task",
			args:    []string{"create", "nope", "--repo", "o/r", "--head", "b"},
			wantRC:  1,
			wantMsg: "not found",
			explain: "must fail before opening a PR it could never link",
		},
		{
			name:    "task already has a PR",
			args:    []string{"create", "TASKID", "--repo", "o/r", "--head", "b"},
			withPR:  7,
			wantRC:  1,
			wantMsg: "already has PR #7",
			explain: "a re-run must not open a second PR for the same task",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := task.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			mgr := task.NewManager(store, nil)
			created, err := mgr.Create("pr probe", "", task.AgentModeHeadless)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tt.withPR > 0 {
				if _, err := mgr.Update(created.ID, task.Update{PRNumber: &tt.withPR}); err != nil {
					t.Fatalf("Update: %v", err)
				}
			}
			args := make([]string, len(tt.args))
			for i, a := range tt.args {
				if a == "TASKID" {
					a = created.ID
				}
				args[i] = a
			}

			var rc int
			out := captureStderr(t, func() { rc = cmdPR(mgr, nil, args, true) })

			if rc != tt.wantRC {
				t.Errorf("cmdPR(%v) = %d, want %d — %s", args, rc, tt.wantRC, tt.explain)
			}
			if !strings.Contains(out, tt.wantMsg) {
				t.Errorf("cmdPR(%v) stderr = %q, want it to mention %q — %s",
					args, strings.TrimSpace(out), tt.wantMsg, tt.explain)
			}
		})
	}
}
