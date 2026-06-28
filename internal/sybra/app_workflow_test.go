package sybra

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
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

func TestAgentAdapterExperiencePromptPlanOnly(t *testing.T) {
	store, err := experience.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	adapter := &agentAdapter{
		experience: store,
		agentOrch:  &AgentOrchestrator{cfg: &config.Config{Experience: config.ExperienceConfig{Enabled: true, MaxRecords: 5}}},
	}
	tk := task.Task{ID: "current", ProjectID: "owner/repo"}

	cfg := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&cfg, agent.RolePlan, tk)
	if !strings.Contains(cfg.Prompt, "Verified Experience Memory") || !strings.Contains(cfg.Prompt, "Use focused tests") {
		t.Fatalf("plan prompt missing experience appendix:\n%s", cfg.Prompt)
	}

	nonPlan := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&nonPlan, agent.RoleReview, tk)
	if nonPlan.Prompt != "base" {
		t.Fatalf("non-plan prompt = %q, want unchanged", nonPlan.Prompt)
	}

	adapter.agentOrch.cfg.Experience.Enabled = false
	disabled := agent.RunConfig{Prompt: "base"}
	adapter.withExperiencePrompt(&disabled, agent.RolePlan, tk)
	if disabled.Prompt != "base" {
		t.Fatalf("disabled prompt = %q, want unchanged", disabled.Prompt)
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
