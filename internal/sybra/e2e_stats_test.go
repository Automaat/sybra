//go:build !short

package sybra

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestE2E_Stats_RecordedOnAgentComplete verifies that running a real agent
// (fake-claude / fake-codex binary) results in a persisted stats record with
// correct cost, token counts, outcome, and task/provider metadata.
func TestE2E_Stats_RecordedOnAgentComplete(t *testing.T) {
	for _, tc := range []struct {
		provider     string
		scenario     string
		wantCost     float64
		wantIn       int
		wantOut      int
		wantOutcome  string
		wantProvider string
	}{
		{
			provider:     "claude",
			scenario:     "success",
			wantCost:     0.01,
			wantIn:       100,
			wantOut:      50,
			wantOutcome:  "completed",
			wantProvider: "claude",
		},
		{
			provider:     "codex",
			scenario:     "success",
			wantCost:     0,
			wantIn:       100,
			wantOut:      20,
			wantOutcome:  "completed",
			wantProvider: "codex",
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			binDir := buildTestBinaries(t)
			t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
			t.Setenv("FAKE_CLAUDE_SCENARIO", tc.scenario)
			t.Setenv("FAKE_CODEX_SCENARIO", tc.scenario)

			home, err := os.MkdirTemp("", "sybra-stats-e2e-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			t.Setenv("SYBRA_HOME", home)

			tasksDir := filepath.Join(home, "tasks")
			if err := os.MkdirAll(tasksDir, 0o755); err != nil {
				t.Fatal(err)
			}

			statsPath := filepath.Join(home, "stats.json")
			statsStore, err := stats.NewStore(statsPath)
			if err != nil {
				t.Fatal(err)
			}

			store, err := task.NewStore(tasksDir)
			if err != nil {
				t.Fatal(err)
			}
			taskMgr := task.NewManager(store, nil)

			logDir, err := os.MkdirTemp("", "sybra-stats-e2e-logs-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(logDir) })

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			logger := e2eLogger()
			agentMgr := agent.NewManager(ctx, func(string, any) {}, logger, logDir)
			agentMgr.SetDefaultProvider(tc.provider)

			wtDir := t.TempDir()
			wm := worktree.New(worktree.Config{
				WorktreesDir: wtDir,
				Tasks:        taskMgr,
				Logger:       logger,
				AgentChecker: agentMgr.HasRunningAgentForTask,
			})
			agentOrch := newAgentOrchestrator(taskMgr, nil, agentMgr, nil, logger, wm, nil)

			app := &App{
				tasks:     taskMgr,
				agents:    agentMgr,
				worktrees: wm,
				agentOrch: agentOrch,
				logger:    logger,
				stats:     statsStore,
			}

			done := make(chan struct{})
			agentMgr.SetOnComplete(func(ag *agent.Agent) {
				app.onAgentComplete(ag)
				close(done)
			})

			tk, err := taskMgr.Create("stats e2e", "", "headless")
			if err != nil {
				t.Fatal(err)
			}

			workDir := t.TempDir()
			ag, err := agentMgr.Run(agent.RunConfig{
				TaskID: tk.ID,
				Prompt: "hello",
				Mode:   "headless",
				Dir:    workDir,
			})
			if err != nil {
				t.Fatal(err)
			}

			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("agent %s did not complete within 30s", ag.ID)
			}

			resp := statsStore.Query()
			if resp.AllTime.TotalRuns != 1 {
				t.Fatalf("expected 1 stat record, got %d", resp.AllTime.TotalRuns)
			}
			r := resp.RecentRuns[0]

			if r.CostUSD != tc.wantCost {
				t.Errorf("CostUSD = %f, want %f", r.CostUSD, tc.wantCost)
			}
			if r.InputTokens != tc.wantIn {
				t.Errorf("InputTokens = %d, want %d", r.InputTokens, tc.wantIn)
			}
			if r.OutputTokens != tc.wantOut {
				t.Errorf("OutputTokens = %d, want %d", r.OutputTokens, tc.wantOut)
			}
			if r.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", r.Outcome, tc.wantOutcome)
			}
			if r.TaskID != tk.ID {
				t.Errorf("TaskID = %q, want %q", r.TaskID, tk.ID)
			}
			if r.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", r.Provider, tc.wantProvider)
			}

			// Verify persistence: reload from disk and confirm record survives.
			reloaded, err := stats.NewStore(statsPath)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Len() != 1 {
				t.Fatalf("after reload: expected 1 record, got %d", reloaded.Len())
			}
		})
	}
}
