package agentorch

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
)

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

func newProjectStoreWithProject(t *testing.T, id, owner, repo string) *project.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := project.NewStore(dir, t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	yamlBody := "id: " + id + "\nname: " + repo + "\nowner: " + owner + "\nrepo: " + repo +
		"\nurl: https://github.com/" + owner + "/" + repo + "\nclone_path: /tmp/" + repo + ".git\ntype: pet\n"
	fileName := strings.ReplaceAll(id, "/", "--") + ".yaml"
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write project fixture: %v", err)
	}
	return store
}

// TestOrchestrator_TestIsolationEnv pins the SYBRA_HOME injection this task
// adds (sybra#1558): a test-runner/eval agent testing Sybra against itself
// must get an isolated SYBRA_HOME, or a second instance can fight the
// production one over ~/.sybra (reattaching to live agents, advancing other
// tasks' workflows). Every other project must be unaffected.
func TestOrchestrator_TestIsolationEnv(t *testing.T) {
	t.Parallel()

	t.Run("sybra project gets an isolated SYBRA_HOME", func(t *testing.T) {
		t.Parallel()
		projects := newProjectStoreWithProject(t, "Automaat/sybra", "Automaat", "sybra")
		o := New(nil, projects, nil, nil, slog.New(slog.DiscardHandler), nil, nil)
		o.SetSandboxes(sandbox.NewManager(t.TempDir(), slog.New(slog.DiscardHandler)))

		env := o.TestIsolationEnv("task-1", task.Task{ProjectID: "Automaat/sybra"})
		if len(env) != 1 || !strings.HasPrefix(env[0], "SYBRA_HOME=") {
			t.Fatalf("TestIsolationEnv = %v, want a single SYBRA_HOME entry", env)
		}
		dir := strings.TrimPrefix(env[0], "SYBRA_HOME=")
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("SYBRA_HOME dir %q was not created: %v", dir, err)
		}
	})

	t.Run("non-sybra project is untouched", func(t *testing.T) {
		t.Parallel()
		projects := newProjectStoreWithProject(t, "owner/repo", "owner", "repo")
		o := New(nil, projects, nil, nil, slog.New(slog.DiscardHandler), nil, nil)
		o.SetSandboxes(sandbox.NewManager(t.TempDir(), slog.New(slog.DiscardHandler)))

		if env := o.TestIsolationEnv("task-1", task.Task{ProjectID: "owner/repo"}); env != nil {
			t.Fatalf("TestIsolationEnv = %v, want nil for a non-sybra project", env)
		}
	})

	t.Run("no project id is a no-op", func(t *testing.T) {
		t.Parallel()
		o := New(nil, nil, nil, nil, slog.New(slog.DiscardHandler), nil, nil)
		o.SetSandboxes(sandbox.NewManager(t.TempDir(), slog.New(slog.DiscardHandler)))

		if env := o.TestIsolationEnv("task-1", task.Task{}); env != nil {
			t.Fatalf("TestIsolationEnv = %v, want nil without a project id", env)
		}
	})

	t.Run("no sandboxes manager wired is a no-op", func(t *testing.T) {
		t.Parallel()
		projects := newProjectStoreWithProject(t, "Automaat/sybra", "Automaat", "sybra")
		o := New(nil, projects, nil, nil, slog.New(slog.DiscardHandler), nil, nil)

		if env := o.TestIsolationEnv("task-1", task.Task{ProjectID: "Automaat/sybra"}); env != nil {
			t.Fatalf("TestIsolationEnv = %v, want nil before sandboxes are wired", env)
		}
	})
}
