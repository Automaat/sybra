package agentorch

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestResolveSandboxMode pins the escape-hatch/default precedence: a task
// can only opt OUT of the configured OS-level sandbox posture (Sandbox:
// false -> "off"), never opt into a stricter posture than configured
// (Sandbox: true is a no-op, matching an escape hatch rather than a
// per-task override).
func TestResolveSandboxMode(t *testing.T) {
	t.Parallel()
	trueVal, falseVal := true, false
	cases := []struct {
		name string
		t    task.Task
		cfg  *config.Config
		want string
	}{
		{"no override, fresh install default", task.Task{}, &config.Config{}, "report"},
		{"no override, configured enforce", task.Task{}, &config.Config{Agent: config.AgentDefaults{SandboxMode: "enforce"}}, "enforce"},
		{"escape hatch off", task.Task{Sandbox: &falseVal}, &config.Config{Agent: config.AgentDefaults{SandboxMode: "enforce"}}, "off"},
		{"sandbox=true is a no-op, config wins", task.Task{Sandbox: &trueVal}, &config.Config{Agent: config.AgentDefaults{SandboxMode: "enforce"}}, "enforce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveSandboxMode(tc.t, tc.cfg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTaskCumulativeCostUSD verifies the sum feeding the
// agent.max_task_cost_usd gate: every AgentRun's CostUSD counts, regardless
// of provider or outcome, and an empty run history sums to zero rather than
// panicking or blocking dispatch.
func TestTaskCumulativeCostUSD(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		runs []task.AgentRun
		want float64
	}{
		{name: "no runs", runs: nil, want: 0},
		{name: "single run", runs: []task.AgentRun{{CostUSD: 4.5}}, want: 4.5},
		{
			name: "sums across providers and outcomes",
			runs: []task.AgentRun{
				{Provider: "claude", CostUSD: 5.0, State: "stopped"},
				{Provider: "codex", CostUSD: 3.25, State: "stopped"},
				{Provider: "claude", CostUSD: 0, State: "running"},
			},
			want: 8.25,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := taskCumulativeCostUSD(tc.runs); got != tc.want {
				t.Errorf("taskCumulativeCostUSD() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStartAgentWithAssignment_TaskCostExceededBlocksDispatch verifies the
// per-task cumulative USD budget gate: once a task's recorded AgentRuns.CostUSD
// sum meets agent.max_task_cost_usd, StartAgentWithAssignment must refuse to
// start another agent instead of dispatching yet another run (each individually
// under the per-run MaxCostUSD cap, but unbounded in aggregate). The gate must
// fire before any worktree/dispatch work — proven here by the task having no
// project_id and no worktree manager, which would otherwise surface a
// different (worktree-related) error.
func TestStartAgentWithAssignment_TaskCostExceededBlocksDispatch(t *testing.T) {
	t.Parallel()

	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	created, err := tm.Create("cost-capped task", "", "headless")
	if err != nil {
		t.Fatalf("task Create: %v", err)
	}
	if err := tm.AddRun(created.ID, task.AgentRun{AgentID: "a1", Provider: "claude", CostUSD: 4.0, State: "stopped"}); err != nil {
		t.Fatalf("AddRun 1: %v", err)
	}
	if err := tm.AddRun(created.ID, task.AgentRun{AgentID: "a2", Provider: "claude", CostUSD: 4.5, State: "stopped"}); err != nil {
		t.Fatalf("AddRun 2: %v", err)
	}

	am, err := agent.NewManager(t.Context(), func(string, any) {}, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
	})
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}

	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{MaxTaskCostUSD: 8.0},
	})

	_, _, err = o.StartAgentWithAssignment(created.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{})
	if err == nil {
		t.Fatal("expected dispatch to be refused once cumulative task cost meets the cap, got nil error")
	}
	if !errors.Is(err, workflow.ErrTaskCostExceeded) {
		t.Fatalf("err = %v, want wrapping workflow.ErrTaskCostExceeded", err)
	}
	reason, permanent := workflow.ClassifyAgentStartError(err)
	if !permanent {
		t.Error("task-cost-exceeded must classify as permanent so the resume loop stops retrying")
	}
	if !strings.Contains(reason, "task cumulative cost exceeds") {
		t.Errorf("reason = %q, missing task-cost explanation", reason)
	}
}

// TestStartAgentWithAssignment_PropagatesOutputSchema is the regression guard
// for #2235's second finding: outputSchema used to be silently dropped on
// this path (the common implementation-role, no-pre-staged-worktree
// dispatch that agentAdapter.StartAgent delegates to), so a future
// implementation-role step declaring output_schema would hit the exact
// receipt-vs-schema conflict this issue fixed, with none of that fix's
// protection engaging since Agent.OutputSchema would stay empty.
func TestStartAgentWithAssignment_PropagatesOutputSchema(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 1)

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
		},
	})

	tk := newAgentTask(t, tm, "schema-enforced dispatch")
	schema := `{"type":"object","properties":{"verdict":{"type":"string"}}}`
	ag, _, err := o.StartAgentWithAssignment(tk.ID, "headless", "go", false, false, "", schema, workflow.AgentAssignment{})
	if err != nil {
		t.Fatalf("StartAgentWithAssignment: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(tk.ID, 5*time.Second) })
	if ag.OutputSchema != schema {
		t.Fatalf("Agent.OutputSchema = %q, want %q", ag.OutputSchema, schema)
	}
}

func TestStartPRFixAgent_TaskCostExceededBlocksDispatch(t *testing.T) {
	t.Parallel()

	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	created, err := tm.Create("cost-capped pr-fix task", "", "headless")
	if err != nil {
		t.Fatalf("task Create: %v", err)
	}
	if err := tm.AddRun(created.ID, task.AgentRun{AgentID: "a1", Provider: "claude", CostUSD: 8.0, State: "stopped"}); err != nil {
		t.Fatalf("AddRun: %v", err)
	}

	am, err := agent.NewManager(t.Context(), func(string, any) {}, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
	})
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}

	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{MaxTaskCostUSD: 8.0},
	})

	err = o.StartPRFixAgent(created.ID)
	if err == nil {
		t.Fatal("expected pr-fix dispatch to be refused once cumulative task cost meets the cap, got nil error")
	}
	if !errors.Is(err, workflow.ErrTaskCostExceeded) {
		t.Fatalf("err = %v, want wrapping workflow.ErrTaskCostExceeded", err)
	}
}

// TestStartPRFixAgent_PoolBusyTranslatesToWorkflowSentinel pins the fix for
// the lost_agent false-positive on task 3e2f0953: recovery.Recovery calls
// StartPRFixAgent directly (internal/recovery/stale.go), bypassing the
// workflow-engine agentAdapter that previously was the only caller
// translating agent.ErrMaxConcurrentReached into workflow.ErrAgentPoolBusy.
// Without translating at the source, a benign, self-healing "pool full"
// condition reached workflow.ClassifyAgentStartError as a raw error it
// doesn't recognize, which fell through to the generic "agent start failed"
// branch and wrote a scary, non-suppressed status_reason for a task that was
// only waiting for a slot to free — not one whose agent actually died.
func TestStartPRFixAgent_PoolBusyTranslatesToWorkflowSentinel(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)

	// Fake claude CLI on PATH so the first dispatch can actually "start" and
	// saturate the pool without spending real model credits or failing with
	// an exec-not-found error on a machine without the CLI installed.
	fakebin := t.TempDir()
	fakeClaude := filepath.Join(fakebin, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/usr/bin/env bash\n"+
		"printf '{\"type\":\"system\",\"session_id\":\"fake-session\"}\\n'\n"+
		"sleep 5\n"+
		"printf '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fake-session\",\"result\":\"done\",\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}\\n'\n"),
		0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	am, err := agent.NewManager(t.Context(), func(string, any) {}, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude", MaxConcurrent: 1},
		SandboxHome: func(string) (string, error) {
			return t.TempDir(), nil
		},
	})
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}

	bare, _ := initConflictingRepo(t)
	projDir := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projStore, err := project.NewStore(projDir, filepath.Join(t.TempDir(), "clones"))
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	proj := project.Project{
		ID:        "test/proj",
		Name:      "proj",
		Owner:     "test",
		Repo:      "proj",
		URL:       bare,
		ClonePath: bare,
		Type:      project.ProjectTypePet,
		Status:    project.ProjectStatusReady,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	writeTestProject(t, projDir, proj)
	wtMgr := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Projects:     projStore,
		Tasks:        tm,
		Logger:       discardSlogLogger(),
		LogsDir:      t.TempDir(),
		SetupTimeout: 30 * time.Second,
	})

	// Disabling require_permissions avoids the "needs a running approval
	// server" error a gated headless run would hit first.
	noPermissions := false
	o := New(tm, projStore, am, nil, discardSlogLogger(), wtMgr, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
		},
	})

	first, err := tm.Create("occupies the only pool slot", "", "headless")
	if err != nil {
		t.Fatalf("task Create (first): %v", err)
	}
	if _, err := tm.Update(first.ID, task.Update{ProjectID: task.Ptr(proj.ID)}); err != nil {
		t.Fatalf("task Update (first project): %v", err)
	}
	if err := o.StartPRFixAgent(first.ID); err != nil {
		t.Fatalf("StartPRFixAgent(first) unexpected err: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(first.ID, 5*time.Second) })

	second, err := tm.Create("hits the concurrency cap", "", "headless")
	if err != nil {
		t.Fatalf("task Create (second): %v", err)
	}
	if _, err := tm.Update(second.ID, task.Update{ProjectID: task.Ptr(proj.ID)}); err != nil {
		t.Fatalf("task Update (second project): %v", err)
	}
	err = o.StartPRFixAgent(second.ID)
	if err == nil {
		t.Fatal("expected pool-busy error once the single slot is saturated, got nil")
	}
	if !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("err = %v, want wrapping workflow.ErrAgentPoolBusy", err)
	}
	reason, permanent := workflow.ClassifyAgentStartError(err)
	if reason != "" {
		t.Errorf("reason = %q, want suppressed (empty) for a benign, self-healing pool-busy condition", reason)
	}
	if permanent {
		t.Error("pool-busy must never classify as permanent — a slot frees on its own")
	}
}

// newFakeClaudeManager builds an agent.Manager backed by a fake, long-sleeping
// "claude" CLI on PATH so a test can genuinely saturate agents.TryReserveSlot
// with maxConcurrent, without spending real model credits or requiring the
// claude CLI to be installed. Mirrors the harness in
// TestStartPRFixAgent_PoolBusyTranslatesToWorkflowSentinel.
func newFakeClaudeManager(t *testing.T, maxConcurrent int) *agent.Manager {
	t.Helper()
	fakebin := t.TempDir()
	fakeClaude := filepath.Join(fakebin, "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/usr/bin/env bash\n"+
		"printf '{\"type\":\"system\",\"session_id\":\"fake-session\"}\\n'\n"+
		"sleep 5\n"+
		"printf '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fake-session\",\"result\":\"done\",\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}\\n'\n"),
		0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))

	am, err := agent.NewManager(t.Context(), func(string, any) {}, discardSlogLogger(), t.TempDir(), agent.ManagerConfig{
		Runtime: agent.ManagerRuntimeConfig{DefaultProvider: "claude", MaxConcurrent: maxConcurrent},
		SandboxHome: func(string) (string, error) {
			return t.TempDir(), nil
		},
	})
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	return am
}

func newAgentTask(t *testing.T, tm *task.Manager, title string) task.Task {
	t.Helper()
	tk, err := tm.Create(title, "", "headless")
	if err != nil {
		t.Fatalf("task Create(%q): %v", title, err)
	}
	return tk
}

// TestStartAgentWithAssignment_AdmissionQueueOnPoolBusy pins the workflow implementation
// dispatch path's admission-queue behavior once the agent pool is saturated:
// a pool-busy dispatch is offered to the queue (not just hard-errored), a
// re-dispatch of the same task refreshes the existing queue entry instead of
// duplicating it, and a manual/direct StartAgent call persists a manual queue
// replay and returns a synthetic queued agent instead of hard-erroring.
func TestStartAgentWithAssignment_AdmissionQueueOnPoolBusy(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 1)

	q, err := agentqueue.New(t.TempDir(), agentqueue.Options{}, discardSlogLogger())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
		},
	})
	o.SetQueue(q)

	first := newAgentTask(t, tm, "occupies the only pool slot")
	if _, _, err := o.StartAgentWithAssignment(first.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
		t.Fatalf("StartAgentWithAssignment(first) unexpected err: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(first.ID, 5*time.Second) })

	second := newAgentTask(t, tm, "pool-busy, falls back to the queue")
	_, _, err = o.StartAgentWithAssignment(second.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{})
	if !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("StartAgentWithAssignment(second) err = %v, want wrapping workflow.ErrAgentPoolBusy", err)
	}
	snap := q.Snapshot()
	if len(snap) != 1 || snap[0].TaskID != second.ID {
		t.Fatalf("queue snapshot = %+v, want exactly [%s] queued", snap, second.ID)
	}
	if snap[0].Role != string(agent.RoleImplementation) {
		t.Errorf("queued Role = %q, want %q", snap[0].Role, agent.RoleImplementation)
	}

	// Re-dispatching the same still-pool-busy task must refresh the existing
	// queue entry in place (agentqueue.Offer's dedup contract), not add a
	// second entry.
	_, _, err = o.StartAgentWithAssignment(second.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{})
	if !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("re-dispatch(second) err = %v, want wrapping workflow.ErrAgentPoolBusy", err)
	}
	if got := len(q.Snapshot()); got != 1 {
		t.Fatalf("queue depth after re-dispatch = %d, want 1 (dedup refresh, not a duplicate)", got)
	}

	// A manual/direct StartAgent call must persist a manual replay item and
	// return a synthetic queued agent without consuming a live slot.
	third := newAgentTask(t, tm, "manual dispatch queues durably")
	ag, err := o.StartAgent(third.ID, "headless", "go", true, false)
	if err != nil {
		t.Fatalf("StartAgent(third) unexpected err: %v", err)
	}
	if ag == nil || ag.State != agent.StateQueued {
		t.Fatalf("StartAgent(third) = %+v, want synthetic queued agent", ag)
	}
	if _, err := am.GetAgent(ag.ID); err == nil {
		t.Fatalf("synthetic queued agent %q must not be registered as a live agent", ag.ID)
	}
	if got := am.RunningCount(); got != 1 {
		t.Fatalf("RunningCount after queued manual start = %d, want 1 (queued item must not consume a slot)", got)
	}
	snap = q.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("queue snapshot after manual StartAgent = %+v, want workflow + manual items", snap)
	}
	var manualItem *agentqueue.Item
	for i := range snap {
		if snap[i].TaskID == third.ID {
			manualItem = &snap[i]
			break
		}
	}
	if manualItem == nil {
		t.Fatalf("manual queued item for %s missing from snapshot %+v", third.ID, snap)
	}
	if !manualItem.Manual || manualItem.Mode != "headless" || manualItem.Prompt != "go" || !manualItem.IncludeTaskDescription {
		t.Fatalf("manual queued item = %+v, want Manual=true mode=headless prompt=go includeTaskDescription=true", *manualItem)
	}
}

// TestStartAgentWithAssignment_MaxDepthRejectsWithoutPoolBusySentinel pins
// that a queue at agent.queue.max_depth capacity rejects a genuinely new
// task outright, rather than reporting it as workflow.ErrAgentPoolBusy — an
// unqueued task must never be parked by run_agent as if it were queued.
func TestStartAgentWithAssignment_MaxDepthRejectsWithoutPoolBusySentinel(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 1)

	q, err := agentqueue.New(t.TempDir(), agentqueue.Options{MaxDepth: 1}, discardSlogLogger())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
		},
	})
	o.SetQueue(q)

	first := newAgentTask(t, tm, "occupies the only pool slot")
	if _, _, err := o.StartAgentWithAssignment(first.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
		t.Fatalf("StartAgentWithAssignment(first) unexpected err: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(first.ID, 5*time.Second) })

	second := newAgentTask(t, tm, "fills the queue to max depth")
	if _, _, err := o.StartAgentWithAssignment(second.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("StartAgentWithAssignment(second) err = %v, want wrapping workflow.ErrAgentPoolBusy", err)
	}

	third := newAgentTask(t, tm, "rejected: queue already at max depth")
	_, _, err = o.StartAgentWithAssignment(third.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{})
	if err == nil {
		t.Fatal("expected a non-nil error once the queue is at max depth")
	}
	if errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("err = %v, must NOT wrap workflow.ErrAgentPoolBusy — the task was rejected, not queued", err)
	}
	snap := q.Snapshot()
	if len(snap) != 1 || snap[0].TaskID != second.ID {
		t.Fatalf("queue snapshot = %+v, want unchanged [%s] (rejected task must not appear queued)", snap, second.ID)
	}
}

func TestStartAgent_MaxDepthRejectsManualQueue(t *testing.T) {
	ts, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tm := task.NewManager(ts, nil)
	am := newFakeClaudeManager(t, 1)

	q, err := agentqueue.New(t.TempDir(), agentqueue.Options{MaxDepth: 1}, discardSlogLogger())
	if err != nil {
		t.Fatalf("agentqueue.New: %v", err)
	}

	noPermissions := false
	o := New(tm, nil, am, nil, discardSlogLogger(), nil, &config.Config{
		Agent: config.AgentDefaults{
			ResearchMachineDir: t.TempDir(),
			RequirePermissions: &noPermissions,
		},
	})
	o.SetQueue(q)

	blocker := newAgentTask(t, tm, "occupies the only pool slot")
	if _, _, err := o.StartAgentWithAssignment(blocker.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); err != nil {
		t.Fatalf("StartAgentWithAssignment(blocker) unexpected err: %v", err)
	}
	t.Cleanup(func() { am.KillAgentsForTask(blocker.ID, 5*time.Second) })

	firstQueued := newAgentTask(t, tm, "fills the only queue slot")
	if _, _, err := o.StartAgentWithAssignment(firstQueued.ID, "headless", "go", false, false, "", "", workflow.AgentAssignment{}); !errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("StartAgentWithAssignment(firstQueued) err = %v, want workflow.ErrAgentPoolBusy", err)
	}

	manual := newAgentTask(t, tm, "manual queue rejection")
	ag, err := o.StartAgent(manual.ID, "headless", "go", false, false)
	if err == nil {
		t.Fatalf("StartAgent(manual) = %+v, want non-nil error once queue is at max depth", ag)
	}
	if errors.Is(err, workflow.ErrAgentPoolBusy) {
		t.Fatalf("StartAgent(manual) err = %v, want hard rejection rather than workflow.ErrAgentPoolBusy", err)
	}
	snap := q.Snapshot()
	if len(snap) != 1 || snap[0].TaskID != firstQueued.ID {
		t.Fatalf("queue snapshot = %+v, want unchanged [%s]", snap, firstQueued.ID)
	}
}

// TestPickImplementationResumeSession pins two regression guards on the
// resume-session walker:
//
//  1. Cross-role pollution: triage/plan/eval session_ids must never be
//     handed to the implementation agent, even when they are the most
//     recent run on the task. Claude CLI bails with
//     "error_during_execution" because the session lives in a different
//     cwd.
//  2. Cross-workflow pollution: an aborted implementation run from a
//     prior workflow execution must not leak its session_id into a fresh
//     execution. The session_id no longer exists in claude's session
//     store, so claude exits with "No conversation found", cost $0, and
//     verify_commits flips the task to human-required without ever
//     running the implementation prompt.
//  3. Cross-provider pollution: a retry dispatched on a different provider
//     than the run that created the session must never adopt that session
//     id. A codex-created session_id is meaningless to claude's session
//     store (and vice versa), so resuming it fails instantly with
//     "No conversation found", cost $0, before the retry's prompt is ever
//     sent.
func TestPickImplementationResumeSession(t *testing.T) {
	t.Parallel()

	wfStart := time.Now()

	cases := []struct {
		name          string
		runs          []task.AgentRun
		workflowStart time.Time
		provider      string
		want          string
	}{
		{
			name: "empty",
			runs: nil,
			want: "",
		},
		{
			name: "only triage with session — must not resume",
			runs: []task.AgentRun{
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "",
		},
		{
			name: "triage then implementation — return implementation",
			runs: []task.AgentRun{
				{Role: "triage", SessionID: "ses-triage"},
				{Role: string(agent.RoleImplementation), SessionID: "ses-impl"},
			},
			want: "ses-impl",
		},
		{
			name: "implementation then triage — skip triage, return impl",
			runs: []task.AgentRun{
				{Role: string(agent.RoleImplementation), SessionID: "ses-impl-1"},
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "ses-impl-1",
		},
		{
			name: "explicit implementation role",
			runs: []task.AgentRun{
				{Role: string(agent.RoleImplementation), SessionID: "ses-impl-explicit"},
			},
			want: "ses-impl-explicit",
		},
		{
			name: "skip empty session_id, return previous impl",
			runs: []task.AgentRun{
				{Role: string(agent.RoleImplementation), SessionID: "ses-old"},
				{Role: string(agent.RoleImplementation), SessionID: ""},
			},
			want: "ses-old",
		},
		{
			name: "non-impl roles only — never resume",
			runs: []task.AgentRun{
				{Role: "plan", SessionID: "ses-plan"},
				{Role: "eval", SessionID: "ses-eval"},
				{Role: "triage", SessionID: "ses-triage"},
			},
			want: "",
		},
		{
			name: "legacy empty-Role run still picked when no time cutoff",
			runs: []task.AgentRun{
				{Role: "", SessionID: "ses-legacy"},
			},
			want: "ses-legacy",
		},
		{
			name: "stale impl from prior workflow — must NOT resume",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-stale",
					StartedAt: wfStart.Add(-24 * time.Hour),
				},
			},
			workflowStart: wfStart,
			want:          "",
		},
		{
			name: "stale empty-Role impl from prior workflow — must NOT resume",
			runs: []task.AgentRun{
				{
					Role:      "",
					SessionID: "ses-stale-empty",
					StartedAt: wfStart.Add(-24 * time.Hour),
				},
			},
			workflowStart: wfStart,
			want:          "",
		},
		{
			name: "current-workflow impl preferred over stale impl",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-stale",
					StartedAt: wfStart.Add(-24 * time.Hour),
				},
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-current",
					StartedAt: wfStart.Add(time.Minute),
				},
			},
			workflowStart: wfStart,
			want:          "ses-current",
		},
		{
			name: "run started exactly at workflow start is eligible",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-edge",
					StartedAt: wfStart,
				},
			},
			workflowStart: wfStart,
			want:          "ses-edge",
		},
		{
			name: "codex session must not resume on a claude retry",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-codex",
					Provider:  "codex",
				},
			},
			provider: "claude",
			want:     "",
		},
		{
			name: "codex then claude failover — return claude session, not codex",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-codex",
					Provider:  "codex",
					StartedAt: wfStart,
				},
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-claude",
					Provider:  "claude",
					StartedAt: wfStart.Add(time.Minute),
				},
				{
					// Failed instant-bail retry left no usable session — falls
					// through to the still-eligible claude run above it.
					Role:      string(agent.RoleImplementation),
					SessionID: "",
					Provider:  "claude",
					StartedAt: wfStart.Add(2 * time.Minute),
				},
			},
			workflowStart: wfStart,
			provider:      "claude",
			want:          "ses-claude",
		},
		{
			name: "provider match — same-provider session still resumes",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-claude",
					Provider:  "claude",
				},
			},
			provider: "claude",
			want:     "ses-claude",
		},
		{
			name: "legacy empty-Provider run still resumes regardless of dispatch provider",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-legacy-provider",
				},
			},
			provider: "claude",
			want:     "ses-legacy-provider",
		},
		{
			name: "no provider context — filter disabled, most recent impl wins",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-codex",
					Provider:  "codex",
				},
			},
			want: "ses-codex",
		},
		{
			name: "one zero-output stall (below threshold) — still resumes",
			runs: []task.AgentRun{
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-poison",
					ResumeZeroOutputStall: true,
				},
			},
			want: "ses-poison",
		},
		{
			name: "two consecutive zero-output stalls (at threshold) — session poisoned, fresh session",
			runs: []task.AgentRun{
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-poison",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart,
				},
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-poison",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart.Add(time.Minute),
				},
			},
			want: "",
		},
		{
			name: "two stalls then a later non-stall run of same session — resumes, never poisoned",
			runs: []task.AgentRun{
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-recovered",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart,
				},
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-recovered",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart.Add(time.Minute),
				},
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-recovered",
					StartedAt: wfStart.Add(2 * time.Minute),
				},
			},
			want: "ses-recovered",
		},
		{
			name: "newest session poisoned, older distinct session still qualifying — falls back to older",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-older",
					StartedAt: wfStart,
				},
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-newer-poisoned",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart.Add(time.Minute),
				},
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-newer-poisoned",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart.Add(2 * time.Minute),
				},
			},
			want: "ses-older",
		},
		{
			name: "newest session sub-threshold stall + older distinct qualifying session — resumes newest",
			runs: []task.AgentRun{
				{
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-older",
					StartedAt: wfStart,
				},
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-newer",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart.Add(time.Minute),
				},
			},
			want: "ses-newer",
		},
		{
			name: "non-stall rate-limited run of same session never counts toward the streak",
			runs: []task.AgentRun{
				{
					Role:                  string(agent.RoleImplementation),
					SessionID:             "ses-mixed",
					ResumeZeroOutputStall: true,
					StartedAt:             wfStart,
				},
				{
					// A different (non-zero-output) rate-limit stop: buildRunPatch
					// never sets ResumeZeroOutputStall for these, so this run must
					// resolve the candidate immediately rather than extend the streak.
					Role:      string(agent.RoleImplementation),
					SessionID: "ses-mixed",
					StartedAt: wfStart.Add(time.Minute),
				},
			},
			want: "ses-mixed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PickImplementationResumeSession(tc.runs, tc.workflowStart, tc.provider)
			if got != tc.want {
				t.Errorf("PickImplementationResumeSession() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildTaskStartPrompt(t *testing.T) {
	t.Parallel()

	taskData := task.Task{Title: "My task", Body: "Task body"}

	got := BuildTaskStartPrompt(taskData, "do the thing", false)
	if got != "do the thing" {
		t.Fatalf("BuildTaskStartPrompt(include=false) = %q, want %q", got, "do the thing")
	}

	got = BuildTaskStartPrompt(taskData, "do the thing", true)
	want := "# Task: My task\n\nTask body\n\n---\n\ndo the thing"
	if got != want {
		t.Fatalf("BuildTaskStartPrompt(include=true) = %q, want %q", got, want)
	}

	got = BuildTaskStartPrompt(taskData, "   \n\t", true)
	if !strings.Contains(got, "# Task: My task") {
		t.Fatalf("BuildTaskStartPrompt(include=true, empty prompt) = %q, want task context", got)
	}
}

// TestAutoAssignProject pins the fix for a project-less task never dispatching
// on a machine with more than one registered project: without an explicit
// agent.default_project_id, auto-assignment only fires for the sole-project
// case (unchanged legacy behavior); with it configured and the ID present in
// the registered set, it wins regardless of how many projects exist. An
// unregistered/typo'd default_project_id must not force a bogus assignment.
func TestAutoAssignProject(t *testing.T) {
	t.Parallel()

	newStores := func(t *testing.T) (*task.Manager, *project.Store) {
		t.Helper()
		ts, err := task.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("task.NewStore: %v", err)
		}
		ps, err := project.NewStore(t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatalf("project.NewStore: %v", err)
		}
		return task.NewManager(ts, nil), ps
	}

	t.Run("no-op when project already set", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{})
		got, err := o.AutoAssignProject(task.Task{ID: "t1", ProjectID: "owner/repo"})
		if err != nil {
			t.Fatalf("AutoAssignProject() err = %v, want nil", err)
		}
		if got.ProjectID != "owner/repo" {
			t.Fatalf("ProjectID = %q, want unchanged", got.ProjectID)
		}
	})

	t.Run("sole project auto-assigns without config", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		if _, err := ps.CreateMeta("https://github.com/owner/solo", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		created, err := tm.Create("t", "b", "headless")
		if err != nil {
			t.Fatalf("task Create: %v", err)
		}
		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{})
		got, err := o.AutoAssignProject(created)
		if err != nil {
			t.Fatalf("AutoAssignProject() err = %v, want nil", err)
		}
		if got.ProjectID != "owner/solo" {
			t.Fatalf("ProjectID = %q, want %q", got.ProjectID, "owner/solo")
		}
	})

	t.Run("multiple projects without default_project_id stays unassigned", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		if _, err := ps.CreateMeta("https://github.com/owner/one", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		if _, err := ps.CreateMeta("https://github.com/owner/two", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		created, err := tm.Create("t", "b", "headless")
		if err != nil {
			t.Fatalf("task Create: %v", err)
		}
		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{})
		got, err := o.AutoAssignProject(created)
		if err != nil {
			t.Fatalf("AutoAssignProject() err = %v, want nil", err)
		}
		if got.ProjectID != "" {
			t.Fatalf("ProjectID = %q, want empty (ambiguous, no default configured)", got.ProjectID)
		}
	})

	t.Run("multiple projects with configured default_project_id wins", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		if _, err := ps.CreateMeta("https://github.com/owner/one", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		if _, err := ps.CreateMeta("https://github.com/owner/two", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		created, err := tm.Create("t", "b", "headless")
		if err != nil {
			t.Fatalf("task Create: %v", err)
		}
		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{Agent: config.AgentDefaults{DefaultProjectID: "owner/two"}})
		got, err := o.AutoAssignProject(created)
		if err != nil {
			t.Fatalf("AutoAssignProject() err = %v, want nil", err)
		}
		if got.ProjectID != "owner/two" {
			t.Fatalf("ProjectID = %q, want %q", got.ProjectID, "owner/two")
		}
	})

	t.Run("unregistered default_project_id is a no-op", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		if _, err := ps.CreateMeta("https://github.com/owner/one", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		if _, err := ps.CreateMeta("https://github.com/owner/two", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		created, err := tm.Create("t", "b", "headless")
		if err != nil {
			t.Fatalf("task Create: %v", err)
		}
		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{Agent: config.AgentDefaults{DefaultProjectID: "owner/typo"}})
		got, err := o.AutoAssignProject(created)
		if err != nil {
			t.Fatalf("AutoAssignProject() err = %v, want nil", err)
		}
		if got.ProjectID != "" {
			t.Fatalf("ProjectID = %q, want empty (default_project_id not registered)", got.ProjectID)
		}
	})

	t.Run("project list error is returned", func(t *testing.T) {
		t.Parallel()
		projectDir := t.TempDir()
		tm, err := task.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("task.NewStore: %v", err)
		}
		ps, err := project.NewStore(projectDir, t.TempDir())
		if err != nil {
			t.Fatalf("project.NewStore: %v", err)
		}
		if err := os.RemoveAll(projectDir); err != nil {
			t.Fatalf("RemoveAll(projectDir): %v", err)
		}
		if err := os.WriteFile(projectDir, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("WriteFile(projectDir): %v", err)
		}
		o := New(task.NewManager(tm, nil), ps, nil, nil, discardSlogLogger(), nil, &config.Config{})
		got, err := o.AutoAssignProject(task.Task{ID: "t1"})
		if err == nil {
			t.Fatal("AutoAssignProject() err = nil, want project list error")
		}
		if got.ProjectID != "" {
			t.Fatalf("ProjectID = %q, want empty after list error", got.ProjectID)
		}
	})
	t.Run("persist failure returns error and leaves input task unchanged", func(t *testing.T) {
		t.Parallel()
		tm, ps := newStores(t)
		if _, err := ps.CreateMeta("https://github.com/owner/solo", project.ProjectTypePet); err != nil {
			t.Fatalf("CreateMeta: %v", err)
		}
		created, err := tm.Create("t", "b", "headless")
		if err != nil {
			t.Fatalf("task Create: %v", err)
		}
		taskDir := filepath.Dir(created.FilePath)
		if err := os.Chmod(taskDir, 0o500); err != nil {
			t.Fatalf("Chmod(taskDir): %v", err)
		}
		defer func() {
			_ = os.Chmod(taskDir, 0o700)
		}()

		o := New(tm, ps, nil, nil, discardSlogLogger(), nil, &config.Config{})
		got, err := o.AutoAssignProject(created)
		if err == nil {
			t.Fatal("AutoAssignProject() err = nil, want persist error")
		}
		if got.ProjectID != "" {
			t.Fatalf("ProjectID = %q, want unchanged input task after persist failure", got.ProjectID)
		}
		stored, getErr := tm.Get(created.ID)
		if getErr != nil {
			t.Fatalf("Get(created.ID): %v", getErr)
		}
		if stored.ProjectID != "" {
			t.Fatalf("stored ProjectID = %q, want empty after persist failure", stored.ProjectID)
		}
	})
}

// TestLogSandboxEscapeHatchRecordsReason pins the accountability half of the
// escape hatch: disabling the sandbox hands a task's agents unrestricted
// write access, so the audit event must carry the operator's stated reason —
// and must flag its absence, so an unexplained bypass is greppable rather
// than merely present.
func TestLogSandboxEscapeHatchRecordsReason(t *testing.T) {
	t.Parallel()
	falseVal, trueVal := false, true
	cases := []struct {
		name       string
		task       task.Task
		wantEvents int
		wantReason string
	}{
		{
			name:       "sandbox unset logs nothing",
			task:       task.Task{},
			wantEvents: 0,
		},
		{
			name:       "sandbox true logs nothing",
			task:       task.Task{Sandbox: &trueVal},
			wantEvents: 0,
		},
		{
			name:       "reason is recorded",
			task:       task.Task{Sandbox: &falseVal, SandboxOffReason: "docker-in-docker e2e needs host mounts"},
			wantEvents: 1,
			wantReason: "docker-in-docker e2e needs host mounts",
		},
		{
			name:       "missing reason is flagged, not silently allowed",
			task:       task.Task{Sandbox: &falseVal},
			wantEvents: 1,
			wantReason: "",
		},
		{
			name:       "whitespace-only reason counts as missing",
			task:       task.Task{Sandbox: &falseVal, SandboxOffReason: "   "},
			wantEvents: 1,
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			al, err := audit.NewLogger(dir)
			if err != nil {
				t.Fatalf("audit.NewLogger: %v", err)
			}
			o := New(nil, nil, nil, al, slog.New(slog.DiscardHandler), nil,
				&config.Config{Agent: config.AgentDefaults{SandboxMode: "enforce"}})

			o.logSandboxEscapeHatch("task-1", tc.task)

			events := readAuditEvents(t, dir)
			if len(events) != tc.wantEvents {
				t.Fatalf("got %d audit events, want %d", len(events), tc.wantEvents)
			}
			if tc.wantEvents == 0 {
				return
			}
			e := events[0]
			if e.Type != audit.EventAgentSandboxDisabled {
				t.Errorf("event type = %q, want %q", e.Type, audit.EventAgentSandboxDisabled)
			}
			if got, _ := e.Data["reason"].(string); got != tc.wantReason {
				t.Errorf("reason = %q, want %q", got, tc.wantReason)
			}
			if got, _ := e.Data["configured_default"].(string); got != "enforce" {
				t.Errorf("configured_default = %q, want %q", got, "enforce")
			}
		})
	}
}

func readAuditEvents(t *testing.T, dir string) []audit.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var events []audit.Event
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var e audit.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			events = append(events, e)
		}
	}
	return events
}
