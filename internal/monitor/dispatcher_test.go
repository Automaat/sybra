package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
)

// recordingRunner captures every RunConfig handed to it and returns a fixed
// Agent stub. Used by dispatcher_test to assert exact wiring.
type recordingRunner struct {
	mu    sync.Mutex
	calls []agent.RunConfig
	err   error
}

func (r *recordingRunner) Run(cfg agent.RunConfig) (*agent.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cfg)
	if r.err != nil {
		return nil, r.err
	}
	return &agent.Agent{ID: "stub-agent"}, nil
}

func (r *recordingRunner) last() agent.RunConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return agent.RunConfig{}
	}
	return r.calls[len(r.calls)-1]
}

// dispatcherTasksStub is a taskAPI fake that returns one known task for Get
// and records no state. Used to test worktree resolution.
type dispatcherTasksStub struct {
	task task.Task
	err  error
}

func (d dispatcherTasksStub) List() ([]task.Task, error) { return nil, nil }
func (d dispatcherTasksStub) Get(id string) (task.Task, error) {
	if d.err != nil {
		return task.Task{}, d.err
	}
	if id != d.task.ID {
		return task.Task{}, errors.New("not found")
	}
	return d.task, nil
}
func (d dispatcherTasksStub) Update(string, task.Update) (task.Task, error) {
	return task.Task{}, errors.New("not implemented")
}

func newTestDispatcher(runner agentRunner, tasks taskAPI, worktreeFn func(task.Task) (string, bool)) *agentDispatcher {
	return &agentDispatcher{
		agents:       runner,
		tasks:        tasks,
		worktreePath: worktreeFn,
		repoDir:      "/repo",
		model:        "sonnet",
	}
}

func TestDispatcher_BoardWideAnomalyRunsInRepoDir(t *testing.T) {
	rr := &recordingRunner{}
	d := newTestDispatcher(rr, nil, nil)

	a := Anomaly{
		Kind:        KindFailureSpike,
		Fingerprint: "failure_spike",
		Evidence: map[string]any{
			"failure_rate": 0.5,
			"agent_runs":   10,
		},
	}
	agentID, err := d.Dispatch(context.Background(), a)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if agentID != "stub-agent" {
		t.Errorf("agent id: got %q", agentID)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("want 1 Run call, got %d", len(rr.calls))
	}
	cfg := rr.last()
	if cfg.TaskID != "" {
		t.Errorf("board-wide anomaly should have empty TaskID, got %q", cfg.TaskID)
	}
	if cfg.Dir != "/repo" {
		t.Errorf("dir: want /repo, got %q", cfg.Dir)
	}
	if cfg.Mode != "headless" {
		t.Errorf("mode: want headless, got %q", cfg.Mode)
	}
	if cfg.Model != "sonnet" {
		t.Errorf("model: want sonnet, got %q", cfg.Model)
	}
	if !cfg.IgnoreConcurrencyLimit {
		t.Errorf("IgnoreConcurrencyLimit must be true for monitor dispatches")
	}
	if !equalStrings(cfg.AllowedTools, []string{"Bash", "Read"}) {
		t.Errorf("allowed tools: want [Bash Read], got %v", cfg.AllowedTools)
	}
	if !strings.Contains(cfg.Prompt, "failure-spike investigator") {
		t.Errorf("prompt missing kind-specific text: %q", cfg.Prompt)
	}
}

func TestDispatcher_PRGapUsesWorktreePath(t *testing.T) {
	rr := &recordingRunner{}
	theTask := task.Task{ID: "abc123", Title: "Fix login", ProjectID: "owner/repo"}
	tasks := dispatcherTasksStub{task: theTask}
	worktreeFn := func(tt task.Task) (string, bool) {
		if tt.ID != theTask.ID {
			return "", false
		}
		return "/worktrees/fix-login-abc123", true
	}
	d := newTestDispatcher(rr, tasks, worktreeFn)

	a := Anomaly{
		Kind:        KindPRGap,
		TaskID:      "abc123",
		Fingerprint: "pr_gap:abc123",
		Evidence: map[string]any{
			"task_id": "abc123",
			"title":   "Fix login",
		},
	}
	if _, err := d.Dispatch(context.Background(), a); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	cfg := rr.last()
	if cfg.TaskID != "abc123" {
		t.Errorf("task id: want abc123, got %q", cfg.TaskID)
	}
	if cfg.Dir != "/worktrees/fix-login-abc123" {
		t.Errorf("dir: want worktree, got %q", cfg.Dir)
	}
	if !strings.Contains(cfg.Prompt, "PR-gap remediator") {
		t.Errorf("prompt missing pr-gap text: %q", cfg.Prompt)
	}
	if !strings.Contains(cfg.Name, "pr_gap") {
		t.Errorf("name missing kind: %q", cfg.Name)
	}
}

func TestDispatcher_PRGapFallsBackToRepoDirWhenWorktreeMissing(t *testing.T) {
	rr := &recordingRunner{}
	theTask := task.Task{ID: "abc123", Title: "Fix login", ProjectID: "owner/repo"}
	tasks := dispatcherTasksStub{task: theTask}
	worktreeFn := func(task.Task) (string, bool) { return "", false }
	d := newTestDispatcher(rr, tasks, worktreeFn)

	a := Anomaly{Kind: KindPRGap, TaskID: "abc123", Fingerprint: "pr_gap:abc123"}
	if _, err := d.Dispatch(context.Background(), a); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if rr.last().Dir != "/repo" {
		t.Errorf("want fallback to /repo, got %q", rr.last().Dir)
	}
}

func TestDispatcher_TaskGetErrorFallsBackToRepoDir(t *testing.T) {
	rr := &recordingRunner{}
	tasks := dispatcherTasksStub{err: errors.New("boom")}
	d := newTestDispatcher(rr, tasks, nil)

	a := Anomaly{Kind: KindStuckHumanBlocked, TaskID: "ghost", Fingerprint: "stuck:ghost"}
	if _, err := d.Dispatch(context.Background(), a); err != nil {
		t.Fatalf("dispatch should not error on task lookup failure: %v", err)
	}
	cfg := rr.last()
	if cfg.Dir != "/repo" {
		t.Errorf("want /repo on task lookup failure, got %q", cfg.Dir)
	}
	if cfg.TaskID != "ghost" {
		t.Errorf("task id must survive even when Get fails, got %q", cfg.TaskID)
	}
}

func TestDispatcher_RunnerErrorPropagates(t *testing.T) {
	rr := &recordingRunner{err: errors.New("max concurrent")}
	d := newTestDispatcher(rr, nil, nil)

	a := Anomaly{Kind: KindFailureSpike, Fingerprint: "failure_spike"}
	_, err := d.Dispatch(context.Background(), a)
	if err == nil {
		t.Fatal("expected runner error to propagate")
	}
	if !strings.Contains(err.Error(), "failure_spike") {
		t.Errorf("error should mention anomaly kind: %v", err)
	}
}

func TestDispatcher_EmptyRepoDirRejected(t *testing.T) {
	rr := &recordingRunner{}
	d := &agentDispatcher{agents: rr, repoDir: ""}

	a := Anomaly{Kind: KindFailureSpike, Fingerprint: "failure_spike"}
	_, err := d.Dispatch(context.Background(), a)
	if err == nil {
		t.Fatal("expected error when repoDir unresolved")
	}
	if len(rr.calls) != 0 {
		t.Error("runner must not be called when dir unresolved")
	}
}

// Compile-time assertion: *agent.Manager satisfies agentRunner. If the
// upstream Run signature ever changes, this line fails before runtime.
var _ agentRunner = (*agent.Manager)(nil)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
