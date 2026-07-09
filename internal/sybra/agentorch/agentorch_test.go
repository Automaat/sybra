package agentorch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
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
