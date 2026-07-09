package sybra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

func TestEligibleRerequestReviewer(t *testing.T) {
	tests := []struct {
		name     string
		login    string
		viewer   string
		author   string
		expected bool
	}{
		{name: "comment author", login: "alice", viewer: "me", author: "author", expected: true},
		{name: "empty", login: "", viewer: "me", author: "author", expected: false},
		{name: "viewer", login: "me", viewer: "me", author: "author", expected: false},
		{name: "pr author", login: "author", viewer: "me", author: "author", expected: false},
		{name: "bot", login: "renovate[bot]", viewer: "me", author: "author", expected: false},
		{name: "case-insensitive viewer", login: "Me", viewer: "me", author: "author", expected: false},
		{name: "case-insensitive author", login: "Author", viewer: "me", author: "author", expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eligibleRerequestReviewer(tt.login, tt.viewer, tt.author)
			if got != tt.expected {
				t.Fatalf("eligibleRerequestReviewer(%q, %q, %q) = %v, want %v",
					tt.login, tt.viewer, tt.author, got, tt.expected)
			}
		})
	}
}

func TestAgentAdapterExperiencePromptPlanAndTriageOnly(t *testing.T) {
	tmp := t.TempDir()
	store, err := experience.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects := newExperienceProjectStore(t, tmp)
	seedExperienceProject(t, filepath.Join(tmp, "projects"), project.Project{
		ID:    "owner/repo",
		Owner: "owner",
		Repo:  "repo",
		URL:   "https://github.com/owner/repo",
		Type:  project.ProjectTypePet,
	})
	if err := store.Put("owner/repo", experience.Record{
		TaskID:      "task-old",
		ProjectID:   "owner/repo",
		ProjectType: "pet",
		Outcome:     "merged",
		Title:       "Use focused tests",
		Strategy:    "Keep it narrow",
	}); err != nil {
		t.Fatal(err)
	}
	orchCfg := &config.Config{Experience: config.ExperienceConfig{Enabled: true, MaxRecords: 5}}
	adapter := &agentAdapter{
		experience: store,
		agentOrch:  agentorch.New(nil, projects, nil, nil, nil, nil, orchCfg),
	}
	tk := task.Task{ID: "current", ProjectID: "owner/repo"}

	cfg := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&cfg, agent.RolePlan, tk)
	if !strings.Contains(cfg.Prompt, "Verified Experience Memory") || !strings.Contains(cfg.Prompt, "Use focused tests") {
		t.Fatalf("plan prompt missing experience appendix:\n%s", cfg.Prompt)
	}

	triage := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&triage, agent.RoleTriage, tk)
	if !strings.Contains(triage.Prompt, "Verified Experience Memory") || !strings.Contains(triage.Prompt, "Use focused tests") {
		t.Fatalf("triage prompt missing experience appendix:\n%s", triage.Prompt)
	}

	nonRetrievalRole := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&nonRetrievalRole, agent.RoleReview, tk)
	if nonRetrievalRole.Prompt != "base" {
		t.Fatalf("non-retrieval role prompt = %q, want unchanged", nonRetrievalRole.Prompt)
	}

	orchCfg.Experience.Enabled = false
	disabled := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&disabled, agent.RolePlan, tk)
	if disabled.Prompt != "base" {
		t.Fatalf("disabled prompt = %q, want unchanged", disabled.Prompt)
	}
}

func TestAgentAdapterExperiencePromptUsesOpaqueWorkKey(t *testing.T) {
	tmp := t.TempDir()
	store, err := experience.New(filepath.Join(tmp, "experience"))
	if err != nil {
		t.Fatal(err)
	}
	projects := newExperienceProjectStore(t, tmp)
	workProject := project.Project{
		ID:    "workco/private",
		Owner: "workco",
		Repo:  "private",
		URL:   "https://github.com/workco/private",
		Type:  project.ProjectTypeWork,
	}
	seedExperienceProject(t, filepath.Join(tmp, "projects"), workProject)
	if err := store.Put(experience.ProjectKey(workProject), experience.Record{
		TaskID: "task-old",
		Title:  "Use narrow tests",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("workco/private", experience.Record{
		TaskID: "raw-key",
		Title:  "must not load",
	}); err != nil {
		t.Fatal(err)
	}
	adapter := &agentAdapter{
		experience: store,
		agentOrch:  agentorch.New(nil, projects, nil, nil, nil, nil, &config.Config{Experience: config.ExperienceConfig{Enabled: true, MaxRecords: 5}}),
	}

	cfg := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&cfg, agent.RolePlan, task.Task{ID: "current", ProjectID: "workco/private"})
	if !strings.Contains(cfg.Prompt, "Use narrow tests") {
		t.Fatalf("work prompt missing opaque-key record:\n%s", cfg.Prompt)
	}
	if strings.Contains(cfg.Prompt, "must not load") {
		t.Fatalf("work prompt loaded raw project-key record:\n%s", cfg.Prompt)
	}
}

func TestManualTestConfigGetterFallsBackToProjectConfigWithoutWorktree(t *testing.T) {
	t.Parallel()

	taskStore, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)
	created, err := taskMgr.Create("exercise manual test config", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	projectID := "owner/repo"
	if _, err := taskMgr.Update(created.ID, task.Update{ProjectID: &projectID}); err != nil {
		t.Fatal(err)
	}

	projectsDir := t.TempDir()
	projects, err := project.NewStore(projectsDir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectYAML := `id: owner/repo
name: repo
owner: owner
repo: repo
url: https://github.com/owner/repo
clone_path: /tmp/repo.git
type: pet
manual_test:
  kind: cli
  command: sybra-cli --json list
`
	if err := os.WriteFile(filepath.Join(projectsDir, "owner--repo.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	getter := &manualTestConfigGetterAdapter{
		tasks:    taskMgr,
		projects: projects,
		mgr:      worktree.New(worktree.Config{WorktreesDir: t.TempDir(), Tasks: taskMgr, Logger: discardLogger()}),
	}
	got := getter.ManualTestConfig(created.ID)
	if got.Kind != "cli" || got.Command != "sybra-cli --json list" {
		t.Fatalf("ManualTestConfig = %+v, want project-level cli config", got)
	}
}

func TestBranchSyncerAdapter_TaskLookupFailureReturnsFailedResult(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	adapter := &branchSyncerAdapter{
		tasks: task.NewManager(store, nil),
		mgr:   &worktree.Manager{},
	}

	result, err := adapter.SyncTaskBranch(context.Background(), "missing-task")
	if result != worktree.SyncFailed.String() {
		t.Fatalf("result = %q, want %q", result, worktree.SyncFailed)
	}
	if err == nil {
		t.Fatal("expected task lookup error")
	}
	if !strings.Contains(err.Error(), "get task") {
		t.Fatalf("err = %v, want get task context", err)
	}
}

// TestAgentAdapterStartAgentSystemRoleHonorsDispatchClaim guards against the
// dispatch-claim race (sybra#1406): a second dispatcher for the same task
// (e.g. a fast ResumeStalled retry) must be turned away with
// ErrDispatchInFlight rather than racing a system-role (plan/eval/triage)
// agent start against an in-flight one. Simulates the race directly by
// pre-holding the claim the way a concurrent dispatcher would, instead of
// spawning real concurrent goroutines against a live provider.
func TestAgentAdapterStartAgentSystemRoleHonorsDispatchClaim(t *testing.T) {
	a := setupApp(t)
	created, err := a.tasks.Create("plan race guard", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}

	if !a.agents.ClaimTaskDispatch(created.ID) {
		t.Fatal("failed to seed dispatch claim")
	}
	defer a.agents.ReleaseTaskDispatch(created.ID)

	agentID, _, _, err := aa.StartAgent(created.ID, string(agent.RolePlan), "headless", "sonnet", "claude", "prompt", "", nil, false, false, "", "", workflow.AgentAssignment{})
	if !errors.Is(err, workflow.ErrDispatchInFlight) {
		t.Fatalf("StartAgent() err = %v, want ErrDispatchInFlight", err)
	}
	if agentID != "" {
		t.Fatalf("StartAgent() agentID = %q, want empty on dispatch-in-flight", agentID)
	}
}

func TestRecordSystemAgentStartIgnoresMissingTask(t *testing.T) {
	t.Parallel()

	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr := task.NewManager(store, nil)
	created, err := mgr.Create("missing task race", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Delete(created.ID); err != nil {
		t.Fatal(err)
	}

	adapter := &agentAdapter{tasks: mgr}
	adapter.recordSystemAgentStart(created.ID, string(agent.RolePlan), "headless", agent.RunConfig{Prompt: "prompt"}, &agent.Agent{
		ID:        "agent-1",
		Provider:  "claude",
		Model:     "sonnet",
		StartedAt: time.Now().UTC(),
	})
}
