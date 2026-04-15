package monitor

import (
	"context"
	"fmt"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
)

// Dispatcher hands an anomaly to a focused headless Claude agent. The agent
// runs the per-kind prompt from prompts.go and is responsible for filing or
// commenting its own GitHub issue. Dispatch is fire-and-forget — the Service
// does not block on agent completion.
type Dispatcher interface {
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

// Dispatch resolves a working directory and a task title for the anomaly,
// then asks agent.Manager to start a headless agent with the per-kind prompt.
// For board-wide anomalies (no taskId) the agent runs in repoDir without a
// task association — the existing manager.Run rejects an empty Dir, so the
// caller must supply a non-empty repoDir.
func (d *agentDispatcher) Dispatch(_ context.Context, a Anomaly) (string, error) {
	dir, taskID, name := d.resolveTarget(a)
	if dir == "" {
		return "", fmt.Errorf("dispatch %s: working directory unresolved", a.Kind)
	}
	cfg := agent.RunConfig{
		TaskID:                 taskID,
		Name:                   name,
		Mode:                   "headless",
		Prompt:                 DispatchPrompt(a, d.issueRepo),
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

// noopDispatcher is used by `synapse-cli monitor scan` and tests that need a
// Dispatcher without spawning real agents.
type noopDispatcher struct{}

func (noopDispatcher) Dispatch(context.Context, Anomaly) (string, error) { return "", nil }

// NoopDispatcher returns a Dispatcher that never spawns a process. Exported
// so the CLI and tests can share the same instance.
func NoopDispatcher() Dispatcher { return noopDispatcher{} }

// noopSink is the IssueSink equivalent for read-only flows.
type noopSink struct{}

func (noopSink) Submit(context.Context, Anomaly, string) (bool, error) { return false, nil }

// NoopSink returns an IssueSink that records nothing.
func NoopSink() IssueSink { return noopSink{} }
