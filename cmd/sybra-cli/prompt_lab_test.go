package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
)

func newTaskManager(t *testing.T, dir string) *task.Manager {
	t.Helper()
	store, err := task.NewStore(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	return task.NewManager(store, task.NoopEmitter())
}

func newProjectStore(t *testing.T, dir string) *project.Store {
	t.Helper()
	ps, err := project.NewStore(filepath.Join(dir, "projects"), filepath.Join(dir, "clones"))
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}
	return ps
}

func testProposal(id, role string, projectIDs []string) promptlab.Proposal {
	return promptlab.Proposal{
		ID:      id,
		Subject: promptlab.Subject{Role: role},
		Title:   "Prompt Lab: tighten instructions for role " + role,
		Rationale: "role " + role + " fails 40% vs 10% overall over 20 runs referencing " +
			"https://github.com/acme/work-repo/pull/9",
		Candidate:             promptlab.VariantCandidate{ID: id, Intent: "tighten-instructions"},
		Evidence:              promptlab.WeakSubject{Subject: promptlab.Subject{Role: role}, ProjectIDs: projectIDs},
		Offline:               promptlab.OfflineResult{Verdict: promptlab.VerdictNoVerdict},
		RequiresHumanApproval: true,
	}
}

func TestFilePromptLabProposalsScrubsWorkTyped(t *testing.T) {
	dir := setupStore(t)
	tasks := newTaskManager(t, dir)
	projects := newProjectStore(t, dir)

	proj, err := projects.CreateMeta("https://github.com/acme/work-repo.git", project.ProjectTypeWork)
	if err != nil {
		t.Fatalf("CreateMeta: %v", err)
	}

	p := testProposal("pl-work-1", "implementation", []string{proj.ID})
	result := promptlab.RunResult{Proposals: []promptlab.Proposal{p}}

	filed, err := filePromptLabProposals(tasks, projects, result)
	if err != nil {
		t.Fatalf("filePromptLabProposals: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("len(filed) = %d, want 1", len(filed))
	}
	body := filed[0].Body
	if strings.Contains(body, proj.Owner) || strings.Contains(body, proj.Repo) || strings.Contains(body, proj.ID) {
		t.Fatalf("work-typed proposal body was not scrubbed: %s", body)
	}
	if strings.Contains(body, "github.com/acme") {
		t.Fatalf("work-typed proposal body still contains a github URL: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("expected redaction placeholder in scrubbed body: %s", body)
	}
	if filed[0].Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", filed[0].Status)
	}
}

func TestFilePromptLabProposalsLeavesPetUnredacted(t *testing.T) {
	dir := setupStore(t)
	tasks := newTaskManager(t, dir)
	projects := newProjectStore(t, dir)

	proj, err := projects.CreateMeta("https://github.com/acme/pet-repo.git", project.ProjectTypePet)
	if err != nil {
		t.Fatalf("CreateMeta: %v", err)
	}

	petProposal := testProposal("pl-pet-1", "implementation", []string{proj.ID})
	nilProjectProposal := testProposal("pl-nil-1", "review", nil)
	result := promptlab.RunResult{Proposals: []promptlab.Proposal{petProposal, nilProjectProposal}}

	filed, err := filePromptLabProposals(tasks, projects, result)
	if err != nil {
		t.Fatalf("filePromptLabProposals: %v", err)
	}
	if len(filed) != 2 {
		t.Fatalf("len(filed) = %d, want 2", len(filed))
	}
	for _, f := range filed {
		if strings.Contains(f.Body, "[redacted]") {
			t.Fatalf("pet/nil-project proposal body was unexpectedly scrubbed: %s", f.Body)
		}
	}
	if !strings.Contains(filed[0].Body, proj.Owner) {
		t.Fatalf("pet-typed proposal body should retain project identifiers: %s", filed[0].Body)
	}
}

func TestFilePromptLabProposalsSkipsDuplicates(t *testing.T) {
	dir := setupStore(t)
	tasks := newTaskManager(t, dir)
	projects := newProjectStore(t, dir)

	p := testProposal("pl-dup-1", "implementation", nil)
	result := promptlab.RunResult{Proposals: []promptlab.Proposal{p}}

	first, err := filePromptLabProposals(tasks, projects, result)
	if err != nil {
		t.Fatalf("first file: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("len(first) = %d, want 1", len(first))
	}
	second, err := filePromptLabProposals(tasks, projects, result)
	if err != nil {
		t.Fatalf("second file: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("len(second) = %d, want 0 (duplicate proposal ID must not refile)", len(second))
	}
}
