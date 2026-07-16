//go:build e2e

package sybra

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/stats"
	"github.com/Automaat/sybra/internal/sybra/completion"
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
			provider: "codex",
			scenario: "success",
			// Codex emits no cost — agent_completion estimates it via
			// stats.EstimateCostDetailed("gpt-5.4", 100 in, 20 out, 0, 0, 0, time.Now())
			// = 100*1.25/1M + 20*10/1M = 0.000325.
			wantCost:     0.000325,
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
			auditDir := filepath.Join(home, "audit")
			auditLogger, err := audit.NewLogger(auditDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = auditLogger.Close() })

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
			done := make(chan struct{})
			var h *completion.Handler
			agentMgr := newTestAgentManager(t, ctx, func(string, any) {}, logger, logDir, agent.ManagerConfig{
				Runtime: agent.ManagerRuntimeConfig{DefaultProvider: tc.provider},
				OnComplete: func(ag *agent.Agent) {
					h.OnComplete(ag)
					close(done)
				},
			})

			wtDir := t.TempDir()
			wm := worktree.New(worktree.Config{
				WorktreesDir: wtDir,
				Tasks:        taskMgr,
				Logger:       logger,
				AgentChecker: agentMgr.HasRunningAgentForTask,
			})
			h = completion.New(completion.Config{
				Logger:    logger,
				Audit:     auditLogger,
				Tasks:     taskMgr,
				Worktrees: wm,
				Stats:     statsStore,
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

			if math.Abs(r.CostUSD-tc.wantCost) > 1e-9 {
				t.Errorf("CostUSD = %g, want %g", r.CostUSD, tc.wantCost)
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

			events, err := audit.Read(auditDir, audit.Query{
				Since: nowMinus(time.Hour),
				Until: time.Now().Add(time.Hour),
				Type:  audit.EventAgentCompleted,
			})
			if err != nil {
				t.Fatal(err)
			}
			summary := audit.Summarize(events, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			if summary.AgentRuns != 1 {
				t.Fatalf("audit summary agent runs = %d, want 1", summary.AgentRuns)
			}
			// Claude emits provider-native cost into the audit event; codex cost
			// is estimated for stats, so the live end-to-end assertion here is
			// the shared terminal-run accounting, not estimated-cost parity.
			if tc.provider == "claude" && math.Abs(summary.TotalCostUSD-tc.wantCost) > 1e-9 {
				t.Fatalf("audit summary cost = %g, want %g", summary.TotalCostUSD, tc.wantCost)
			}
		})
	}
}

func nowMinus(d time.Duration) time.Time {
	return time.Now().Add(-d)
}
