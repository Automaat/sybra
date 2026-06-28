package sybra

import (
	"os"
	"path/filepath"
	"testing"

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
