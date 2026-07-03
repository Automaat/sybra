package monitor

import (
	"context"
	"fmt"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Dispatcher hands an anomaly to a focused headless Claude agent. The agent
// runs the per-kind prompt from prompts.go and is responsible for filing or
// commenting its own GitHub issue. Dispatch is fire-and-forget — the Service
// does not block on agent completion.
type Dispatcher interface {
	// Dispatchable reports whether the anomaly can be dispatched to an agent.
	// Returns (true, "") when dispatchable.
	// Returns (false, "") when undispatchable for an expected reason (e.g. an
	// external GitHub-issue task with no worktree and empty repoDir).
	// Returns (false, reason) for unexpected conditions (task store not
	// configured, transient task lookup failure) — callers should WARN.
	Dispatchable(a Anomaly) (ok bool, skipReason string)
	Dispatch(ctx context.Context, a Anomaly) (agentID string, err error)
}

// agentRunner is the slice of agent.Manager the dispatcher needs. Defining
// it as an interface lets tests inject a recording fake without constructing
// a full Manager. *agent.Manager satisfies this interface naturally.
type agentRunner interface {
	Run(cfg agent.RunConfig) (*agent.Agent, error)
}

// agentDispatcher is the production implementation. It reuses the existing
// agent.Manager so monitor-spawned agents show up in the Agents list, get
// audit events, and respect the same lifecycle as user-initiated headless
// runs.
type agentDispatcher struct {
	agents       agentRunner
	tasks        taskAPI
	worktreePath func(t task.Task) (string, bool)
	repoDir      string
	model        string
	issueRepo    string
}

// AgentDispatcherDeps groups the constructor inputs so the call site at app
// wiring time stays declarative.
type AgentDispatcherDeps struct {
	Agents       *agent.Manager
	Tasks        taskAPI
	WorktreePath func(t task.Task) (string, bool)
	RepoDir      string
	Model        string
	// IssueRepo is the "owner/name" GitHub repository where monitor issues
	// must be filed. Passed explicitly into prompts so agents are independent
	// of their working directory (which may be a task worktree for an
	// unrelated project).
	IssueRepo string
}

func NewAgentDispatcher(d AgentDispatcherDeps) *agentDispatcher {
	return &agentDispatcher{
		agents:       d.Agents,
		tasks:        d.Tasks,
		worktreePath: d.WorktreePath,
		repoDir:      d.RepoDir,
		model:        d.Model,
		issueRepo:    d.IssueRepo,
	}
}

// Dispatchable implements Dispatcher. It calls resolveTarget for the fast path
// (dir non-empty → dispatchable). When dir is empty it classifies the reason:
// expected cases (external task with no worktree) return (false, ""); unexpected
// cases (missing task store, transient lookup failure) return (false, reason) so
// the caller can emit a WARN instead of silently swallowing the anomaly.
func (d *agentDispatcher) Dispatchable(a Anomaly) (ok bool, skipReason string) {
	dir, _, _ := d.resolveTarget(a)
	if dir != "" {
		return true, ""
	}
	// dir is empty — classify the root cause so the caller can distinguish
	// expected external-task skips from unexpected resolution failures.
	if a.TaskID == "" {
		return false, "board-wide anomaly: repoDir not configured"
	}
	if d.tasks == nil {
		return false, "task store not configured"
	}
	if _, err := d.tasks.Get(a.TaskID); err != nil {
		return false, "task lookup failed: " + err.Error()
	}
	// Task exists but has no worktree and repoDir is empty — this is the
	// known external-task case (e.g. GitHub-issue task on a machine with no
	// local clone). Skip silently.
	return false, ""
}

// Dispatch resolves a working directory and a task title for the anomaly,
// then asks agent.Manager to start a headless agent with the per-kind prompt.
// For board-wide anomalies (no taskId) the agent runs in repoDir without a
// task association — the existing manager.Run rejects an empty Dir, so the
// caller must supply a non-empty repoDir.
func (d *agentDispatcher) Dispatch(ctx context.Context, a Anomaly) (string, error) {
	dir, taskID, name := d.resolveTarget(a)
	if dir == "" {
		return "", fmt.Errorf("dispatch %s: working directory unresolved", a.Kind)
	}
	cfg := agent.RunConfig{
		TaskID:                 taskID,
		Name:                   name,
		Mode:                   "headless",
		Prompt:                 DispatchPrompt(a, d.issueRepo, project.PushRemote(ctx, dir)),
		AllowedTools:           []string{"Bash", "Read"},
		Dir:                    dir,
		Model:                  d.model,
		IgnoreConcurrencyLimit: true,
	}
	ag, err := d.agents.Run(cfg)
	if err != nil {
		return "", fmt.Errorf("dispatch %s: %w", a.Kind, err)
	}
	return ag.ID, nil
}

func (d *agentDispatcher) resolveTarget(a Anomaly) (dir, taskID, name string) {
	name = "monitor:" + string(a.Kind)
	if a.TaskID == "" {
		return d.repoDir, "", name
	}
	taskName := name + ":" + a.TaskID
	if d.tasks == nil {
		return d.repoDir, a.TaskID, taskName
	}
	t, err := d.tasks.Get(a.TaskID)
	if err != nil {
		// Fall back to repo dir so dispatch is not blocked by a missing task.
		return d.repoDir, a.TaskID, taskName
	}
	if d.worktreePath != nil {
		if path, ok := d.worktreePath(t); ok {
			return path, a.TaskID, taskName
		}
	}
	return d.repoDir, a.TaskID, taskName
}

// noopDispatcher is used by `sybra-cli monitor scan` and tests that need a
// Dispatcher without spawning real agents.
type noopDispatcher struct{}

func (noopDispatcher) Dispatchable(Anomaly) (ok bool, skipReason string) { return true, "" }
func (noopDispatcher) Dispatch(context.Context, Anomaly) (string, error) { return "", nil }

// NoopDispatcher returns a Dispatcher that never spawns a process. Exported
// so the CLI and tests can share the same instance.
func NoopDispatcher() Dispatcher { return noopDispatcher{} }

// noopSink is the IssueSink equivalent for read-only flows.
type noopSink struct{}

func (noopSink) Submit(context.Context, Anomaly, string) (bool, error) { return false, nil }

// NoopSink returns an IssueSink that records nothing.
func NoopSink() IssueSink { return noopSink{} }
