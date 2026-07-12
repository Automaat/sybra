package sybra

import (
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestCloseLinkedIssueOnDone_ClosesSameRepoIssue(t *testing.T) {
	app, tasks := newUmbrellaGateApp(t)
	var gotRepo, gotComment string
	var gotNum, calls int
	app.umbrellaCloseIssue = func(repo string, number int, comment string) error {
		calls++
		gotRepo, gotNum, gotComment = repo, number, comment
		return nil
	}

	tk, err := tasks.CreateFull("done task", "", task.AgentModeHeadless, task.Update{
		Issue:     task.Ptr("https://github.com/Automaat/sybra/issues/1849"),
		ProjectID: task.Ptr("Automaat/sybra"),
		PRNumber:  task.Ptr(42),
		Status:    task.Ptr(task.StatusDone),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	app.closeLinkedIssueOnDone(tk.ID)

	if calls != 1 || gotRepo != "Automaat/sybra" || gotNum != 1849 {
		t.Fatalf("close called %d times repo=%q num=%d, want 1 Automaat/sybra 1849", calls, gotRepo, gotNum)
	}
	if !strings.Contains(gotComment, "#42") {
		t.Errorf("comment should reference the merging PR, got %q", gotComment)
	}
}

func TestCloseLinkedIssueOnDone_SkipsNoIssueAndCrossRepo(t *testing.T) {
	app, tasks := newUmbrellaGateApp(t)
	calls := 0
	app.umbrellaCloseIssue = func(string, int, string) error { calls++; return nil }

	noIssue, err := tasks.CreateFull("no issue", "", task.AgentModeHeadless, task.Update{
		ProjectID: task.Ptr("Automaat/sybra"),
		Status:    task.Ptr(task.StatusDone),
	})
	if err != nil {
		t.Fatalf("create no-issue: %v", err)
	}
	crossRepo, err := tasks.CreateFull("cross repo", "", task.AgentModeHeadless, task.Update{
		Issue:     task.Ptr("https://github.com/other/repo/issues/9"),
		ProjectID: task.Ptr("Automaat/sybra"),
		Status:    task.Ptr(task.StatusDone),
	})
	if err != nil {
		t.Fatalf("create cross-repo: %v", err)
	}

	app.closeLinkedIssueOnDone(noIssue.ID)
	app.closeLinkedIssueOnDone(crossRepo.ID)

	if calls != 0 {
		t.Errorf("expected no close for missing-issue or cross-repo tasks, got %d calls", calls)
	}
}
