package sybra

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

func TestConflictPrompt_ResolvesAllConflictsAutonomously(t *testing.T) {
	t.Parallel()

	prompt := buildConflictPrompt(github.PullRequest{
		Number:      1178,
		HeadRefName: "fix/example",
	}, "\n\nFiles changed in this PR:\n- internal/workflow/engine.go\n")

	for _, forbidden := range []string{
		"more than 3",
		"run `git rebase --abort` and stop",
		"task needs human review",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("conflict prompt still contains count-based stop instruction %q:\n%s", forbidden, prompt)
		}
	}
	for _, want := range []string{
		"Use the task body, PR diff, changed-file list, and current code as context",
		"Do not stop just because the conflict count is high",
		"resolve all conflicts autonomously",
		"Stop only for a concrete blocker",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("conflict prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestConflictPrompt_UsesMergeNotRebase(t *testing.T) {
	t.Parallel()

	prompt := buildConflictPrompt(github.PullRequest{
		Number:      1178,
		HeadRefName: "fix/example",
	}, "")

	for _, forbidden := range []string{
		"git rebase",
		"force-with-lease",
		"--force",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("conflict prompt still instructs a rebase/force-push workflow (%q):\n%s", forbidden, prompt)
		}
	}
	for _, want := range []string{
		"git merge refs/remotes/origin/main",
		"Do not force-push or rewrite existing commits",
		"nothing to commit",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("conflict prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestStaffCodeReviewRunConfigPinsClaudeProvider(t *testing.T) {
	cfg := staffCodeReviewRunConfig(task.Task{ID: "review-task", Title: "Needs review"}, "Run /staff-code-review", t.TempDir(), "default")

	if cfg.Provider != staffCodeReviewProvider {
		t.Fatalf("Provider = %q, want %q", cfg.Provider, staffCodeReviewProvider)
	}
	if cfg.Model != "opus" {
		t.Fatalf("Model = %q, want opus", cfg.Model)
	}
	if !cfg.DisableProviderFailover {
		t.Fatal("DisableProviderFailover = false, want true")
	}
}

func newExperienceProjectStore(t *testing.T, tmp string) *project.Store {
	t.Helper()
	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	return projects
}

func seedExperienceProject(t *testing.T, dir string, proj project.Project) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if proj.Name == "" {
		proj.Name = proj.Repo
	}
	if proj.ClonePath == "" {
		proj.ClonePath = filepath.Join(t.TempDir(), proj.Repo+".git")
	}
	var b strings.Builder
	b.WriteString("id: " + proj.ID + "\n")
	b.WriteString("name: " + proj.Name + "\n")
	b.WriteString("owner: " + proj.Owner + "\n")
	b.WriteString("repo: " + proj.Repo + "\n")
	b.WriteString("url: " + proj.URL + "\n")
	b.WriteString("clone_path: " + proj.ClonePath + "\n")
	b.WriteString("type: " + string(proj.Type) + "\n")
	if proj.Checks != nil && len(proj.Checks.Verify) > 0 {
		b.WriteString("checks:\n  verify:\n")
		for _, cmd := range proj.Checks.Verify {
			b.WriteString("    - " + cmd + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(proj.ID, "/", "--")+".yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustExperienceStore(t *testing.T, dir string) *experience.Store {
	t.Helper()
	store, err := experience.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func readExperienceAuditEvents(t *testing.T, dir string) []audit.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var events []audit.Event
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var ev audit.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			events = append(events, ev)
		}
	}
	return events
}

// TestPRMonitorEligible exercises the scan predicate used by the PR monitor
// loop. The regression: tasks whose workflow exited to in-progress with a
// live PR number (because an evaluate step crashed, or a manually-spawned
// agent opened the PR outside the workflow) were silently dropped from the
// scan because it only considered status=in-review. Result: failing CI on
// those PRs was never fixed by pr-fix agents.
func TestPRMonitorEligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review with PR — original happy path",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42},
			want: true,
		},
		{
			name: "in-review with branch only — still eligible",
			tk:   task.Task{Status: task.StatusInReview, Branch: "sybra/feat-x"},
			want: true,
		},
		{
			name: "in-review with neither PR nor branch — not eligible",
			tk:   task.Task{Status: task.StatusInReview},
			want: false,
		},
		{
			name: "in-progress with PR — the regression case we're fixing",
			tk:   task.Task{Status: task.StatusInProgress, PRNumber: 247},
			want: true,
		},
		{
			name: "in-progress with branch only — not eligible (avoid WIP false positives)",
			tk:   task.Task{Status: task.StatusInProgress, Branch: "sybra/wip"},
			want: false,
		},
		{
			name: "in-progress with nothing — not eligible",
			tk:   task.Task{Status: task.StatusInProgress},
			want: false,
		},
		{
			name: "review tag excluded (inbound review task, not ours)",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "todo with PR — not eligible, not in monitored states",
			tk:   task.Task{Status: task.StatusTodo, PRNumber: 42},
			want: false,
		},
		{
			name: "done with PR — not eligible, already terminal",
			tk:   task.Task{Status: task.StatusDone, PRNumber: 42},
			want: false,
		},
		{
			name: "human-required with PR — excluded from pr-fix dispatch (see prClosedEligible for auto-done)",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prMonitorEligible(&tt.tk); got != tt.want {
				t.Errorf("prMonitorEligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

func TestPrClosedEligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review with PR — eligible (via prMonitorEligible)",
			tk:   task.Task{Status: task.StatusInReview, PRNumber: 42},
			want: true,
		},
		{
			name: "in-progress with PR — eligible (via prMonitorEligible)",
			tk:   task.Task{Status: task.StatusInProgress, PRNumber: 42},
			want: true,
		},
		{
			name: "human-required with PR — eligible for auto-done when merged",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42},
			want: true,
		},
		{
			name: "human-required with branch only — not eligible (no PR to detect)",
			tk:   task.Task{Status: task.StatusHumanRequired, Branch: "feat"},
			want: false,
		},
		{
			name: "human-required review tag — excluded (inbound review, handled separately)",
			tk:   task.Task{Status: task.StatusHumanRequired, PRNumber: 42, Tags: []string{"review"}},
			want: false,
		},
		{
			name: "human-required chat task — excluded",
			tk:   task.Task{TaskType: task.TaskTypeChat, Status: task.StatusHumanRequired, PRNumber: 42},
			want: false,
		},
		{
			name: "todo with PR — not eligible",
			tk:   task.Task{Status: task.StatusTodo, PRNumber: 42},
			want: false,
		},
		{
			name: "done with PR — not eligible (terminal)",
			tk:   task.Task{Status: task.StatusDone, PRNumber: 42},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := prClosedEligible(&tt.tk); got != tt.want {
				t.Errorf("prClosedEligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

func TestReviewClosedPREligible(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "in-review review task with PR",
			tk: task.Task{
				Status:    task.StatusInReview,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: true,
		},
		{
			name: "human-required review task with PR",
			tk: task.Task{
				Status:    task.StatusHumanRequired,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: true,
		},
		{
			name: "done review task skipped",
			tk: task.Task{
				Status:    task.StatusDone,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
		{
			name: "non-review task skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
		{
			name: "review task without PR skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				Tags:      []string{"review"},
				ProjectID: "o/r",
			},
			want: false,
		},
		{
			name: "chat task skipped",
			tk: task.Task{
				Status:    task.StatusInReview,
				TaskType:  task.TaskTypeChat,
				Tags:      []string{"review"},
				ProjectID: "o/r",
				PRNumber:  42,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewClosedPREligible(&tt.tk); got != tt.want {
				t.Errorf("reviewClosedPREligible(%+v) = %v, want %v", tt.tk, got, tt.want)
			}
		})
	}
}

func TestReviewTaskMatchers(t *testing.T) {
	tasks := []task.Task{
		{ID: "review", Status: task.StatusInReview, Tags: []string{"review"}, ProjectID: "o/r", PRNumber: 42},
		{ID: "done", Status: task.StatusDone, Tags: []string{"review"}, ProjectID: "o/r", PRNumber: 43},
		{ID: "mine", Status: task.StatusInReview, ProjectID: "o/r", PRNumber: 44},
	}

	got := reviewTaskMatchers(tasks)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if got[0].ID != "review" || got[0].ProjectID != "o/r" || got[0].PRNumber != 42 {
		t.Fatalf("matcher = %+v, want review o/r#42", got[0])
	}
}

func TestOpenReviewPRsIncludesApprovedReviews(t *testing.T) {
	requested := github.PullRequest{Number: 1, Repository: "o/r"}
	approved := github.PullRequest{Number: 2, Repository: "o/r", ViewerHasApproved: true}

	got := openReviewPRs(github.ReviewSummary{
		ReviewRequested: []github.PullRequest{requested},
		ReviewedByMe:    []github.PullRequest{approved},
	})

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("numbers = [%d %d], want [1 2]", got[0].Number, got[1].Number)
	}
}

func TestCreateReviewTaskPassesUpdatedTaskToTriage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks")
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	got := make(chan task.Task, 1)

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
	}
	r.createReviewTaskWithTriage(github.PullRequest{
		Number: 2708,
		Title:  "docs: explain precedence",
		URL:    "https://github.com/kumahq/kuma-website/pull/2708",
		Author: "slonka",
	}, "kumahq/kuma-website", func(t task.Task) {
		got <- t
	})

	select {
	case reviewTask := <-got:
		if reviewTask.ProjectID != "kumahq/kuma-website" {
			t.Fatalf("ProjectID = %q, want kumahq/kuma-website", reviewTask.ProjectID)
		}
		if reviewTask.PRNumber != 2708 {
			t.Fatalf("PRNumber = %d, want 2708", reviewTask.PRNumber)
		}
		if reviewTask.Status != task.StatusTodo {
			t.Fatalf("Status = %q, want %q", reviewTask.Status, task.StatusTodo)
		}
	case <-time.After(time.Second):
		t.Fatal("triage was not called")
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("created files = %d, want 1", len(files))
	}
}

func TestReviewAgentAlreadyRan(t *testing.T) {
	tests := []struct {
		name string
		tk   task.Task
		want bool
	}{
		{
			name: "reviewed flag counts",
			tk:   task.Task{Reviewed: true},
			want: true,
		},
		{
			name: "prior review run counts even while running",
			tk: task.Task{AgentRuns: []task.AgentRun{
				{Role: string(agent.RoleReview), State: string(agent.StateRunning)},
			}},
			want: true,
		},
		{
			name: "human diagnostic does not count as staff review",
			tk: task.Task{AgentRuns: []task.AgentRun{
				{Role: string(agent.RoleHumanReview), State: string(agent.StateStopped)},
			}},
			want: false,
		},
		{
			name: "fix-review does not count as staff review",
			tk: task.Task{AgentRuns: []task.AgentRun{
				{Role: string(agent.RoleFixReview), State: string(agent.StateStopped)},
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewAgentAlreadyRan(tt.tk); got != tt.want {
				t.Fatalf("reviewAgentAlreadyRan() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCancelResolvedPRFixWorkflows covers the loop where pr-fix kept
// re-spawning agents long after the underlying CI failure was resolved.
// The fix: cancel any in-flight pr-fix workflow whose pr_issue_kind is no
// longer present in the current MatchTaskPRs output.
func TestAdoptOrphanPRs(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	mk := func(title string, u task.Update) string {
		t.Helper()
		created, cErr := tasks.Create(title, "", string(task.AgentModeHeadless))
		if cErr != nil {
			t.Fatalf("create %s: %v", title, cErr)
		}
		if _, uErr := tasks.Update(created.ID, u); uErr != nil {
			t.Fatalf("update %s: %v", title, uErr)
		}
		return created.ID
	}

	// Stranded by a premature verify_commits verdict: human-required, branch
	// set, no PR number — the exact 94af6462 failure shape.
	orphan := mk("stranded", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("implementation agent failed before committing — no commits on branch"),
		Branch:       task.Ptr("feat/stranded"),
		ProjectID:    task.Ptr("o/r"),
	})
	// Ambiguous: two open PRs in the project share the branch → must NOT adopt.
	ambiguous := mk("ambiguous", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("no commits pushed to branch"),
		Branch:       task.Ptr("feat/ambig"),
		ProjectID:    task.Ptr("o/r"),
	})
	// No matching PR → must NOT adopt.
	noMatch := mk("no-match", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("no commits pushed to branch"),
		Branch:       task.Ptr("feat/orphaned-forever"),
		ProjectID:    task.Ptr("o/r"),
	})
	// Same branch name but the only matching PR is in a DIFFERENT repo → the
	// repo guard must reject it (monitoredPRs spans every repo the user owns).
	wrongRepo := mk("wrong-repo", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("no commits pushed to branch"),
		Branch:       task.Ptr("feat/cross"),
		ProjectID:    task.Ptr("o/r"),
	})
	// Deliberately stopped by the watchdog (not a link-failure strand) → the
	// reason gate must keep adoption from resurrecting it.
	watchdog := mk("watchdog", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("watchdog: runaway loop"),
		Branch:       task.Ptr("feat/wd"),
		ProjectID:    task.Ptr("o/r"),
	})
	// Already in-review with a PR → not eligible, must be left untouched.
	healthy := mk("healthy", task.Update{
		Status:    task.Ptr(task.StatusInReview),
		Branch:    task.Ptr("feat/healthy"),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(42),
	})
	// In-review with no PR → eligible: PR was opened manually but never linked.
	inReviewOrphan := mk("in-review-orphan", task.Update{
		Status:    task.Ptr(task.StatusInReview),
		Branch:    task.Ptr("feat/in-review-orphan"),
		ProjectID: task.Ptr("o/r"),
	})

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{
		{Number: 1051, HeadRefName: "feat/stranded", Repository: "o/r"},
		{Number: 7, HeadRefName: "feat/ambig", Repository: "o/r"},
		{Number: 8, HeadRefName: "feat/ambig", Repository: "o/r"},
		{Number: 9, HeadRefName: "feat/cross", Repository: "o/other-repo"}, // wrong repo
		{Number: 10, HeadRefName: "feat/wd", Repository: "o/r"},            // watchdog task's branch
		{Number: 42, HeadRefName: "feat/healthy", Repository: "o/r"},
		{Number: 1055, HeadRefName: "feat/in-review-orphan", Repository: "o/r"},
	}

	r.adoptOrphanPRs(context.Background(), all, prs)

	t.Run("orphan adopted and re-activated", func(t *testing.T) {
		got, _ := tasks.Get(orphan)
		if got.Status != task.StatusInReview {
			t.Errorf("status = %q, want in-review", got.Status)
		}
		if got.PRNumber != 1051 {
			t.Errorf("PRNumber = %d, want 1051", got.PRNumber)
		}
		if got.StatusReason != "" {
			t.Errorf("StatusReason = %q, want cleared", got.StatusReason)
		}
	})

	t.Run("in-place slice mutation visible to same poll", func(t *testing.T) {
		for i := range all {
			if all[i].ID == orphan {
				if all[i].Status != task.StatusInReview || all[i].PRNumber != 1051 {
					t.Errorf("slice entry not updated in place: status=%q pr=%d", all[i].Status, all[i].PRNumber)
				}
				return
			}
		}
		t.Fatal("orphan not found in slice")
	})

	t.Run("ambiguous branch left untouched", func(t *testing.T) {
		got, _ := tasks.Get(ambiguous)
		if got.Status != task.StatusHumanRequired || got.PRNumber != 0 {
			t.Errorf("ambiguous adopted: status=%q pr=%d, want human-required/0", got.Status, got.PRNumber)
		}
	})

	t.Run("no matching PR left untouched", func(t *testing.T) {
		got, _ := tasks.Get(noMatch)
		if got.Status != task.StatusHumanRequired {
			t.Errorf("no-match adopted: status=%q, want human-required", got.Status)
		}
	})

	t.Run("cross-repo branch collision rejected", func(t *testing.T) {
		got, _ := tasks.Get(wrongRepo)
		if got.Status != task.StatusHumanRequired || got.PRNumber != 0 {
			t.Errorf("cross-repo PR adopted: status=%q pr=%d, want human-required/0", got.Status, got.PRNumber)
		}
	})

	t.Run("watchdog-stopped task not resurrected", func(t *testing.T) {
		got, _ := tasks.Get(watchdog)
		if got.Status != task.StatusHumanRequired || got.PRNumber != 0 {
			t.Errorf("watchdog-stopped task adopted: status=%q pr=%d, want human-required/0", got.Status, got.PRNumber)
		}
	})

	t.Run("in-review orphan linked without status change", func(t *testing.T) {
		got, _ := tasks.Get(inReviewOrphan)
		if got.Status != task.StatusInReview {
			t.Errorf("status = %q, want in-review (must not change)", got.Status)
		}
		if got.PRNumber != 1055 {
			t.Errorf("PRNumber = %d, want 1055", got.PRNumber)
		}
	})

	t.Run("ineligible task untouched", func(t *testing.T) {
		got, _ := tasks.Get(healthy)
		if got.Status != task.StatusInReview || got.PRNumber != 42 {
			t.Errorf("healthy task mutated: status=%q pr=%d", got.Status, got.PRNumber)
		}
	})
}

func TestOrphanPRAdoptionEligible(t *testing.T) {
	const orphanReason = "no commits pushed to branch"
	cases := []struct {
		name string
		t    task.Task
		want bool
	}{
		// human-required cases (strand-reason gate applies)
		{"stranded orphan (no-commits)", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: orphanReason}, true},
		{"evaluate orphan (no PR)", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: "commits pushed but no PR created"}, true},
		{"has pr number", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", PRNumber: 5, StatusReason: orphanReason}, false},
		{"no branch", task.Task{Status: task.StatusHumanRequired, ProjectID: "o/r", StatusReason: orphanReason}, false},
		{"no project", task.Task{Status: task.StatusHumanRequired, Branch: "b", StatusReason: orphanReason}, false},
		{"not human-required or in-review", task.Task{Status: task.StatusInProgress, Branch: "b", ProjectID: "o/r", StatusReason: orphanReason}, false},
		{"watchdog stop not eligible", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: "watchdog: runaway loop"}, false},
		{"unrelated reason not eligible", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: "needs design input"}, false},
		{"review task excluded", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: orphanReason, Tags: []string{"review"}}, false},
		{"chat task excluded", task.Task{Status: task.StatusHumanRequired, Branch: "b", ProjectID: "o/r", StatusReason: orphanReason, TaskType: task.TaskTypeChat}, false},
		// in-review cases (no strand-reason gate: already in review with no PR is unambiguously an orphan)
		{"in-review no PR — eligible", task.Task{Status: task.StatusInReview, Branch: "b", ProjectID: "o/r"}, true},
		{"in-review with PR — not eligible (already linked)", task.Task{Status: task.StatusInReview, Branch: "b", ProjectID: "o/r", PRNumber: 5}, false},
		{"in-review no branch — not eligible", task.Task{Status: task.StatusInReview, ProjectID: "o/r"}, false},
		{"in-review no project — not eligible", task.Task{Status: task.StatusInReview, Branch: "b"}, false},
		{"in-review review tag excluded", task.Task{Status: task.StatusInReview, Branch: "b", ProjectID: "o/r", Tags: []string{"review"}}, false},
		{"in-review chat task excluded", task.Task{Status: task.StatusInReview, Branch: "b", ProjectID: "o/r", TaskType: task.TaskTypeChat}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orphanPRAdoptionEligible(&tc.t); got != tc.want {
				t.Errorf("orphanPRAdoptionEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecordExperienceOnLandingGatesAndWrites(t *testing.T) {
	tmp := t.TempDir()
	projects := newExperienceProjectStore(t, tmp)
	store, err := experience.New(filepath.Join(tmp, "experience"))
	if err != nil {
		t.Fatal(err)
	}
	seedExperienceProject(t, filepath.Join(tmp, "projects"), project.Project{
		ID:    "owner/repo",
		Owner: "owner",
		Repo:  "repo",
		URL:   "https://github.com/owner/repo",
		Type:  project.ProjectTypePet,
	})
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		projects:      projects,
		cfg:           &config.Config{Experience: config.ExperienceConfig{Enabled: true}},
		experience:    store,
	}

	r.recordExperienceOnLanding(task.Task{ID: "closed", ProjectID: "owner/repo", Outcome: "closed"})
	r.recordExperienceOnLanding(task.Task{ID: "merged", ProjectID: "owner/repo", Outcome: "merged", Title: "Keep owner/repo visible"})
	got, err := store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Keep owner/repo visible" {
		t.Fatalf("non-work record = %+v, want unsanitized owner/repo title", got)
	}
	r.recordExperienceOnLanding(task.Task{ID: "merged", ProjectID: "owner/repo", Outcome: "merged", Title: "updated"})

	got, err = store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("records = %+v, want one merged record", got)
	}
	if got[0].TaskID != "merged" || got[0].Title != "updated" {
		t.Fatalf("record = %+v, want idempotently updated merged record", got[0])
	}

	r.cfg.Experience.Enabled = false
	r.recordExperienceOnLanding(task.Task{ID: "disabled", ProjectID: "owner/repo", Outcome: "merged"})
	got, err = store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("disabled feature wrote records: %+v", got)
	}
}

func TestRecordExperienceOnLandingProjectUnresolvedSkips(t *testing.T) {
	tmp := t.TempDir()
	projects := newExperienceProjectStore(t, tmp)
	store, err := experience.New(filepath.Join(tmp, "experience"))
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewLogger(filepath.Join(tmp, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), audit: auditLog},
		projects:      projects,
		cfg:           &config.Config{Experience: config.ExperienceConfig{Enabled: true}},
		experience:    store,
	}

	r.recordExperienceOnLanding(task.Task{ID: "missing", ProjectID: "missing/repo", Outcome: "merged"})
	got, err := store.Query("missing/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unresolved project wrote records: %+v", got)
	}
	events := readExperienceAuditEvents(t, filepath.Join(tmp, "audit"))
	if len(events) != 1 || events[0].Type != audit.EventExperienceSkipped || events[0].Data["reason"] != "project_unresolved" {
		t.Fatalf("audit events = %+v, want experience.skipped project_unresolved", events)
	}
}

func TestRecordExperienceOnLandingScrubsWorkRecords(t *testing.T) {
	tmp := t.TempDir()
	projects := newExperienceProjectStore(t, tmp)
	store, err := experience.New(filepath.Join(tmp, "experience"))
	if err != nil {
		t.Fatal(err)
	}
	auditLog, err := audit.NewLogger(filepath.Join(tmp, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()
	workProject := project.Project{
		ID:    "workco/private",
		Owner: "workco",
		Repo:  "private",
		URL:   "https://github.com/workco/private",
		Type:  project.ProjectTypeWork,
		Checks: &project.ChecksConfig{
			Verify: []string{"go test ./workco/private/..."},
		},
	}
	seedExperienceProject(t, filepath.Join(tmp, "projects"), workProject)
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), audit: auditLog},
		projects:      projects,
		cfg:           &config.Config{Experience: config.ExperienceConfig{Enabled: true}},
		experience:    store,
	}
	r.recordExperienceOnLanding(task.Task{
		ID:        "work",
		ProjectID: "workco/private",
		Outcome:   "merged",
		Title:     "Fix workco/private from https://github.com/workco/private",
		Tags:      []string{"workco"},
		PlanBrief: "Touch private repo",
		AgentRuns: []task.AgentRun{{
			ProtocolViolation: "workco/private appeared",
			TestOutcome:       "workco/private retry",
		}},
	})

	rawDir := filepath.Join(tmp, "experience", "workco--private")
	if _, err := os.Stat(rawDir); !os.IsNotExist(err) {
		t.Fatalf("raw work experience dir exists or stat failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "experience", experience.ProjectKey(workProject), "work.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"workco/private", "workco", "private", "https://github.com/workco/private"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("persisted work record contains %q:\n%s", forbidden, data)
		}
	}
	events := readExperienceAuditEvents(t, filepath.Join(tmp, "audit"))
	auditJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"workco/private", "workco", "private", "https://github.com/workco/private"} {
		if strings.Contains(string(auditJSON), forbidden) {
			t.Fatalf("work experience audit contains %q:\n%s", forbidden, auditJSON)
		}
	}
}

func TestRecordExperienceOnLandingScrubsWorkTaskIDBeforeReuse(t *testing.T) {
	tmp := t.TempDir()
	projects := newExperienceProjectStore(t, tmp)
	store, err := experience.New(filepath.Join(tmp, "experience"))
	if err != nil {
		t.Fatal(err)
	}
	workProject := project.Project{
		ID:    "workco/private",
		Owner: "workco",
		Repo:  "private",
		URL:   "https://github.com/workco/private",
		Type:  project.ProjectTypeWork,
	}
	seedExperienceProject(t, filepath.Join(tmp, "projects"), workProject)
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		projects:      projects,
		cfg:           &config.Config{Experience: config.ExperienceConfig{Enabled: true}},
		experience:    store,
	}

	r.recordExperienceOnLanding(task.Task{ID: "workco", ProjectID: "workco/private", Outcome: "merged", Title: "safe title"})
	r.recordExperienceOnLanding(task.Task{ID: "workco", ProjectID: "workco/private", Outcome: "merged", Title: "updated"})

	records, err := store.Query(experience.ProjectKey(workProject), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one idempotent record", records)
	}
	if records[0].TaskID == "workco" || !strings.HasPrefix(records[0].TaskID, "work-task-") {
		t.Fatalf("TaskID = %q, want opaque work task id", records[0].TaskID)
	}
	if records[0].Title != "updated" {
		t.Fatalf("idempotent update title = %q, want updated", records[0].Title)
	}
	recordJSON, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	prompt := experience.FormatForPrompt(records)
	for _, forbidden := range []string{"workco", "private", "https://github.com/workco/private"} {
		if strings.Contains(string(recordJSON), forbidden) {
			t.Fatalf("persisted work record contains %q:\n%s", forbidden, recordJSON)
		}
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("work identifier reused in planning prompt:\n%s", prompt)
		}
	}
}

func TestRecordExperienceOnLandingWriteErrorIsNonBlocking(t *testing.T) {
	tmp := t.TempDir()
	projects := newExperienceProjectStore(t, tmp)
	seedExperienceProject(t, filepath.Join(tmp, "projects"), project.Project{
		ID:    "owner/repo",
		Owner: "owner",
		Repo:  "repo",
		Type:  project.ProjectTypePet,
	})
	store := mustExperienceStore(t, filepath.Join(tmp, "experience-file"))
	if err := os.RemoveAll(filepath.Join(tmp, "experience-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "experience-file"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		projects:      projects,
		cfg:           &config.Config{Experience: config.ExperienceConfig{Enabled: true}},
		experience:    store,
	}

	r.recordExperienceOnLanding(task.Task{ID: "merged", ProjectID: "owner/repo", Outcome: "merged"})
}

func TestCancelResolvedPRFixWorkflows(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(wfStore); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	// Seed three tasks. task.Create assigns IDs, so map fixture labels
	// to the real generated IDs for the assertions below:
	//   resolved — pr-fix active for ci_failure, no longer in issues → cancel
	//   live     — pr-fix active for conflict, conflict still in issues → leave
	//   other    — no workflow → nothing to do
	ids := map[string]string{}
	mkTask := func(label, kind string, hasWF bool, wfState workflow.ExecState) {
		t.Helper()
		created, err := tasks.Create(label, "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatalf("create %s: %v", label, err)
		}
		ids[label] = created.ID
		pr := 100
		status := task.StatusInProgress
		upd := task.Update{PRNumber: &pr, Status: &status}
		if hasWF {
			wf := &workflow.Execution{
				WorkflowID:  "pr-fix",
				CurrentStep: "fix",
				State:       wfState,
				Variables:   map[string]string{"pr_issue_kind": kind},
			}
			upd.Workflow = &wf
		}
		if _, err := tasks.Update(created.ID, upd); err != nil {
			t.Fatalf("update %s: %v", label, err)
		}
	}
	mkTask("resolved", "ci_failure", true, workflow.ExecWaiting)
	mkTask("live", "conflict", true, workflow.ExecWaiting)
	mkTask("other", "", false, "")

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}

	r := &ReviewHandler{
		DomainHandler:  DomainHandler{logger: logger},
		tasks:          tasks,
		prTracker:      github.NewIssueTracker(time.Minute),
		workflowEngine: engine,
	}
	// "live" task's conflict is still detected. "resolved" task has no
	// matching live issue — that's the case we want to cancel.
	issues := []github.PRIssue{
		{Kind: github.PRIssueConflict, TaskID: ids["live"]},
	}
	// Seed a handled marker so we can check Clear() was called.
	r.prTracker.MarkHandled(ids["resolved"], github.PRIssueCIFailure, "sha-old")

	r.cancelResolvedPRFixWorkflows(all, issues)

	got, err := tasks.Get(ids["resolved"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.State != workflow.ExecCompleted {
		t.Errorf("resolved workflow state = %+v, want completed", got.Workflow)
	}
	if got.Workflow != nil && got.Workflow.CurrentStep != "" {
		t.Errorf("resolved CurrentStep = %q, want empty", got.Workflow.CurrentStep)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("resolved status = %q, want %q", got.Status, task.StatusInReview)
	}
	if !strings.Contains(got.StatusReason, "ci_failure resolved") {
		t.Errorf("resolved status reason = %q, want resolved issue kind", got.StatusReason)
	}
	// Cooldown was cleared, so a future ci_failure on a new SHA can re-trigger.
	if !r.prTracker.ShouldHandle(ids["resolved"], github.PRIssueCIFailure, "sha-new") {
		t.Error("prTracker.Clear was not called for resolved task")
	}

	got, err = tasks.Get(ids["live"])
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.State != workflow.ExecWaiting {
		t.Errorf("live workflow state = %+v, want still waiting", got.Workflow)
	}
	if got.Status != task.StatusInProgress {
		t.Errorf("live status = %q, want %q", got.Status, task.StatusInProgress)
	}
}

// TestCancelResolvedPRFixWorkflows_CoalescedSiblingStillLive is the
// regression guard for the cancel-vs-coalesce interaction: a workflow
// dispatched for {ci_failure, comments} with comments as primary must NOT be
// cancelled just because comments resolved first — ci_failure (recorded in
// the plural pr_issue_kinds var) is still live and the workflow must keep
// running to address it.
func TestCancelResolvedPRFixWorkflows_CoalescedSiblingStillLive(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(wfStore); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	created, err := tasks.Create("coalesced", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	pr := 1352
	status := task.StatusInProgress
	wf := &workflow.Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "fix",
		State:       workflow.ExecWaiting,
		Variables: map[string]string{
			"pr_issue_kind":  string(github.PRIssueComments),
			"pr_issue_kinds": string(github.PRIssueCIFailure) + "," + string(github.PRIssueComments),
		},
	}
	if _, err := tasks.Update(created.ID, task.Update{
		PRNumber: &pr, Status: &status, Workflow: &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}

	r := &ReviewHandler{
		DomainHandler:  DomainHandler{logger: logger},
		tasks:          tasks,
		prTracker:      github.NewIssueTracker(time.Minute),
		workflowEngine: engine,
	}
	r.prTracker.MarkHandled(created.ID, github.PRIssueComments, "sha1")
	r.prTracker.MarkHandled(created.ID, github.PRIssueCIFailure, "sha1")
	// comments has resolved; ci_failure is still live on the same push.
	issues := []github.PRIssue{
		{Kind: github.PRIssueCIFailure, TaskID: created.ID},
	}

	r.cancelResolvedPRFixWorkflows(all, issues)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil || got.Workflow.State != workflow.ExecWaiting {
		t.Errorf("workflow state = %+v, want still waiting while coalesced ci_failure is still live", got.Workflow)
	}
}

// TestMonitoredPRs covers the regression where Renovate-bot PRs linked to a
// task by pr_number were silently skipped by the pr-monitor because
// FetchReviews uses author:@me. monitoredPRs folds renovatePRsFn output into
// the same slice that drives MatchTaskPRs and DetectClosedTaskPRs.
func TestMonitoredPRs(t *testing.T) {
	mine := github.PullRequest{Number: 1, Author: "me"}
	bot := github.PullRequest{Number: 2, Author: "app/renovate"}
	summary := github.ReviewSummary{CreatedByMe: []github.PullRequest{mine}}

	tests := []struct {
		name      string
		fn        func() []github.PullRequest
		wantNums  []int
		wantAlloc bool // true when fn supplies extra PRs (forces a copy)
	}{
		{
			name:     "nil fn returns CreatedByMe directly",
			fn:       nil,
			wantNums: []int{1},
		},
		{
			name:     "empty fn result also returns CreatedByMe directly",
			fn:       func() []github.PullRequest { return nil },
			wantNums: []int{1},
		},
		{
			name:      "fn returns renovate PRs — concatenated after CreatedByMe",
			fn:        func() []github.PullRequest { return []github.PullRequest{bot} },
			wantNums:  []int{1, 2},
			wantAlloc: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReviewHandler{renovatePRsFn: tt.fn}
			got := r.monitoredPRs(summary)
			if len(got) != len(tt.wantNums) {
				t.Fatalf("len = %d, want %d (got %+v)", len(got), len(tt.wantNums), got)
			}
			for i, n := range tt.wantNums {
				if got[i].Number != n {
					t.Errorf("got[%d].Number = %d, want %d", i, got[i].Number, n)
				}
			}
			// When fn adds entries we must return a fresh slice — appending
			// onto summary.CreatedByMe directly would mutate the caller's
			// data on the next poll.
			if tt.wantAlloc && len(got) > 0 && len(summary.CreatedByMe) > 0 &&
				&got[0] == &summary.CreatedByMe[0] {
				t.Error("monitoredPRs aliased summary.CreatedByMe; expected a copy")
			}
		})
	}
}

// TestTriageReviewSmall guards the reviewSmallAdditions (40) and
// reviewSmallFiles (5) thresholds. Both conditions must hold for the PR to be
// routed to human-required; if either meets or exceeds its limit the review
// agent should be dispatched.
func TestTriageReviewSmall(t *testing.T) {
	tests := []struct {
		name      string
		additions int
		files     int
		wantSmall bool
	}{
		// Both strictly below threshold → too small for agent
		{"39 additions, 4 files — below both limits", 39, 4, true},
		{"0 additions, 0 files — zero-sized PR", 0, 0, true},
		{"1 addition, 1 file — minimal PR", 1, 1, true},

		// additions at exact threshold → no longer small (dispatch agent)
		{"40 additions, 4 files — additions at limit", 40, 4, false},
		// files at exact threshold → no longer small (dispatch agent)
		{"39 additions, 5 files — files at limit", 39, 5, false},
		// both at threshold
		{"40 additions, 5 files — both at limit", 40, 5, false},
		// both above threshold
		{"200 additions, 20 files — large PR", 200, 20, false},
		// one well above, one below
		{"100 additions, 1 file — additions above, files below", 100, 1, false},
		{"1 addition, 10 files — additions below, files above", 1, 10, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := triageReviewSmall(tt.additions, tt.files)
			if got != tt.wantSmall {
				t.Errorf("triageReviewSmall(%d, %d) = %v, want %v",
					tt.additions, tt.files, got, tt.wantSmall)
			}
		})
	}
}

// TestPrepareWorktree_CircuitBreaker verifies the stateful failure counter:
// each call that fails worktree creation increments a per-task counter, and
// once that counter reaches wtFailureLimit the task is escalated to
// human-required and the counter is reset.
func TestPrepareWorktree_CircuitBreaker(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	// Create a task with a project_id; PrepareForTask will fail immediately
	// because "owner/repo" is not registered in the project store.
	tk, err := tasks.Create("test task", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	projID := "owner/repo"
	tk, err = tasks.Update(tk.ID, task.Update{ProjectID: task.Ptr(projID)})
	if err != nil {
		t.Fatal(err)
	}

	projStore, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	wt := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(tmp, "worktrees"),
		Projects:     projStore,
		Tasks:        tasks,
		Logger:       slog.New(slog.DiscardHandler),
		LogsDir:      filepath.Join(tmp, "logs"),
	})

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
		worktrees:     wt,
		wtFailures:    make(map[string]int),
	}

	issue := github.PRIssue{
		Kind:   github.PRIssueCIFailure,
		TaskID: tk.ID,
		PR:     github.PullRequest{Number: 1, HeadRefName: "feat/x"},
	}

	// Failures 1–(wtFailureLimit-1): return ("", false) without escalating.
	for i := range wtFailureLimit - 1 {
		_, ok := r.prepareWorktree(context.Background(), tk, issue)
		if ok {
			t.Fatalf("call %d: want ok=false on worktree error", i+1)
		}
		got, err := tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == task.StatusHumanRequired {
			t.Fatalf("call %d: task escalated too early (want %d failures before trip)", i+1, wtFailureLimit)
		}
	}

	// wtFailureLimit-th failure opens the circuit and escalates the task.
	_, ok := r.prepareWorktree(context.Background(), tk, issue)
	if ok {
		t.Fatal("circuit-open call: want ok=false")
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q after circuit break, want human-required", got.Status)
	}
	// Counter must be deleted so the next task start doesn't carry stale state.
	if n := r.wtFailures[tk.ID]; n != 0 {
		t.Fatalf("wtFailures[%s] = %d after circuit trip, want 0", tk.ID, n)
	}
}

// TestAdoptOrphanMergedPR verifies that a task stranded in human-required with
// a known branch is advanced to done when a merged PR is found on that branch
// via the findMergedPRFn hook, and that the slice entry is updated in place.
func TestAdoptOrphanMergedPR(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	mk := func(title string, u task.Update) string {
		t.Helper()
		created, cErr := tasks.Create(title, "", string(task.AgentModeHeadless))
		if cErr != nil {
			t.Fatalf("create %s: %v", title, cErr)
		}
		if _, uErr := tasks.Update(created.ID, u); uErr != nil {
			t.Fatalf("update %s: %v", title, uErr)
		}
		return created.ID
	}

	// Task eligible for orphan adoption: stranded in human-required with a branch.
	orphan := mk("stranded", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("commits pushed but no PR created"),
		Branch:       task.Ptr("feat/stranded"),
		ProjectID:    task.Ptr("o/r"),
	})
	// Already linked — must not be touched.
	linked := mk("linked", task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		PRNumber:  task.Ptr(99),
		Branch:    task.Ptr("feat/linked"),
		ProjectID: task.Ptr("o/r"),
	})

	calls := 0
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
		findMergedPRFn: func(repo, branch string) (int, error) {
			calls++
			if repo == "o/r" && branch == "feat/stranded" {
				return 1051, nil
			}
			return 0, nil
		},
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	// Pass empty open-PR list so all eligible tasks fall through to merged check.
	r.adoptOrphanPRs(context.Background(), all, nil)

	t.Run("orphan advanced to done with merged PR", func(t *testing.T) {
		got, _ := tasks.Get(orphan)
		if got.Status != task.StatusDone {
			t.Errorf("status = %q, want done", got.Status)
		}
		if got.PRNumber != 1051 {
			t.Errorf("PRNumber = %d, want 1051", got.PRNumber)
		}
		if got.Outcome != "merged" {
			t.Errorf("outcome = %q, want merged", got.Outcome)
		}
		if got.StatusReason != "" {
			t.Errorf("StatusReason = %q, want cleared", got.StatusReason)
		}
	})

	t.Run("in-place slice mutation visible to same poll", func(t *testing.T) {
		for i := range all {
			if all[i].ID == orphan {
				if all[i].Status != task.StatusDone || all[i].PRNumber != 1051 {
					t.Errorf("slice entry not updated: status=%q pr=%d", all[i].Status, all[i].PRNumber)
				}
				return
			}
		}
		t.Fatal("orphan not found in slice")
	})

	t.Run("already-linked task not touched", func(t *testing.T) {
		got, _ := tasks.Get(linked)
		if got.Status != task.StatusHumanRequired || got.PRNumber != 99 {
			t.Errorf("linked task mutated: status=%q pr=%d", got.Status, got.PRNumber)
		}
	})

	t.Run("findMergedPRFn called only for eligible orphan", func(t *testing.T) {
		// Only the orphan (no PR number, strand reason) should trigger the lookup.
		if calls != 1 {
			t.Errorf("findMergedPRFn calls = %d, want 1", calls)
		}
	})
}

// TestAdoptOrphanPRs_OpenTakesPrecedence verifies that when an open PR is found
// for an eligible task, the merged-PR fallback is not called.
func TestAdoptOrphanPRs_OpenTakesPrecedence(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("stranded", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("commits pushed but no PR created"),
		Branch:       task.Ptr("feat/my-branch"),
		ProjectID:    task.Ptr("o/r"),
	}); err != nil {
		t.Fatal(err)
	}

	mergedCalled := false
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
		findMergedPRFn: func(_, _ string) (int, error) {
			mergedCalled = true
			return 0, nil
		},
	}

	all, _ := tasks.List()
	openPRs := []github.PullRequest{
		{Number: 42, HeadRefName: "feat/my-branch", Repository: "o/r"},
	}
	r.adoptOrphanPRs(context.Background(), all, openPRs)

	if mergedCalled {
		t.Error("findMergedPRFn was called even though an open PR matched")
	}
	got, _ := tasks.Get(created.ID)
	if got.Status != task.StatusInReview || got.PRNumber != 42 {
		t.Errorf("status=%q pr=%d, want in-review/42", got.Status, got.PRNumber)
	}
}

func TestHasReviewTask_ScopedByProject(t *testing.T) {
	r := &ReviewHandler{}
	existing := []task.Task{
		{ProjectID: "org/a", PRNumber: 42, Tags: []string{"review"}},
		{ProjectID: "org/a", PRNumber: 7, Tags: []string{"backend"}}, // not a review task
	}
	tests := []struct {
		name      string
		projectID string
		prNumber  int
		want      bool
	}{
		{"same project + number is a duplicate", "org/a", 42, true},
		{"same number, different project is not", "org/b", 42, false},
		{"same project, different number is not", "org/a", 99, false},
		{"matching number without review tag is not", "org/a", 7, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.hasReviewTask(existing, tt.projectID, tt.prNumber); got != tt.want {
				t.Errorf("hasReviewTask(%q, %d) = %v, want %v", tt.projectID, tt.prNumber, got, tt.want)
			}
		})
	}
}

// TestCloseFinishedReviewTasks verifies that review tasks whose linked PR is
// merged or closed are advanced to done, and tasks with open or
// inaccessible PRs are left untouched.
//
// Regression: before this fix, closeFinishedReviewTasks was only called when
// FetchReviews() succeeded. A transient summary-fetch failure left any
// in-review review task stranded indefinitely.
func TestCloseFinishedReviewTasks(t *testing.T) {
	tests := []struct {
		name       string
		taskStatus task.Status
		prState    string
		fetchErr   error
		// openPRs lists PRs already known to be open (nil = check all tasks
		// directly, which is what happens on the FetchReviews failure path).
		openPRs    []github.PullRequest
		wantStatus task.Status
	}{
		{
			name:       "merged PR — task advances to done",
			taskStatus: task.StatusInReview,
			prState:    "MERGED",
			wantStatus: task.StatusDone,
		},
		{
			name:       "closed PR — task advances to done",
			taskStatus: task.StatusInReview,
			prState:    "CLOSED",
			wantStatus: task.StatusDone,
		},
		{
			name:       "open PR — task stays in-review",
			taskStatus: task.StatusInReview,
			prState:    "OPEN",
			wantStatus: task.StatusInReview,
		},
		{
			// PR still in the open-PR summary (e.g. GitHub search lag): the
			// function must not re-check it via FetchPRState.
			name:       "PR present in open list — suppressed, stays in-review",
			taskStatus: task.StatusInReview,
			prState:    "MERGED", // fetchPRStateFn would return MERGED, but must not be called
			openPRs:    []github.PullRequest{{Number: 42, Repository: "o/r"}},
			wantStatus: task.StatusInReview,
		},
		{
			// human-required tasks are eligible too (small PR punted to human).
			name:       "human-required review task with merged PR — advances to done",
			taskStatus: task.StatusHumanRequired,
			prState:    "MERGED",
			wantStatus: task.StatusDone,
		},
		{
			// FetchPRState fails transiently; task must be left untouched.
			name:       "fetch error — task stays in-review",
			taskStatus: task.StatusInReview,
			fetchErr:   errors.New("network error"),
			wantStatus: task.StatusInReview,
		},
		{
			// Nil open list replicates the FetchReviews-failure path: every
			// review task's PR is queried directly, so a merged PR is still caught.
			name:       "nil open list (FetchReviews failure path) — merged PR caught",
			taskStatus: task.StatusInReview,
			prState:    "MERGED",
			openPRs:    nil,
			wantStatus: task.StatusDone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
			if err != nil {
				t.Fatal(err)
			}
			tasks := task.NewManager(store, nil)

			created, err := tasks.Create("Review: some PR", "", string(task.AgentModeHeadless))
			if err != nil {
				t.Fatal(err)
			}
			tags := []string{"review"}
			if _, err := tasks.Update(created.ID, task.Update{
				Status:    task.Ptr(tt.taskStatus),
				Tags:      &tags,
				ProjectID: task.Ptr("o/r"),
				PRNumber:  task.Ptr(42),
			}); err != nil {
				t.Fatal(err)
			}

			agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())

			r := &ReviewHandler{
				DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
				tasks:         tasks,
				agents:        agentMgr,
				fetchPRStateFn: func(repo string, number int) (github.PRState, error) {
					if tt.fetchErr != nil {
						return github.PRState{}, tt.fetchErr
					}
					return github.PRState{State: tt.prState}, nil
				},
			}

			all, err := tasks.List()
			if err != nil {
				t.Fatal(err)
			}
			r.closeFinishedReviewTasks(all, tt.openPRs)

			got, err := tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tt.wantStatus)
			}
		})
	}
}

// TestPollAndMonitorPRs_FetchErrorReconcile verifies that stale in-review
// review tasks are reconciled (via FetchPRState) only on transient fetch
// failures, not on non-transient ones (auth, 4xx) where those calls would
// compound backoff against an already-throttled API.
func TestPollAndMonitorPRs_FetchErrorReconcile(t *testing.T) {
	transientErr := errors.New("dial tcp: connection refused")
	nonTransientErr := errors.New("gh: authentication required")

	tests := []struct {
		name          string
		fetchErr      error
		wantReconcile bool // whether fetchPRStateFn should be called
	}{
		{
			name:          "transient error — reconciliation runs",
			fetchErr:      transientErr,
			wantReconcile: true,
		},
		{
			name:          "non-transient error — reconciliation skipped",
			fetchErr:      nonTransientErr,
			wantReconcile: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
			if err != nil {
				t.Fatal(err)
			}
			tasks := task.NewManager(store, nil)

			created, err := tasks.Create("Review: stale PR", "", string(task.AgentModeHeadless))
			if err != nil {
				t.Fatal(err)
			}
			tags := []string{"review"}
			if _, err := tasks.Update(created.ID, task.Update{
				Status:    task.Ptr(task.StatusInReview),
				Tags:      &tags,
				ProjectID: task.Ptr("o/r"),
				PRNumber:  task.Ptr(99),
			}); err != nil {
				t.Fatal(err)
			}

			reconcileCalled := false
			agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
			r := &ReviewHandler{
				DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), emit: func(string, any) {}},
				tasks:         tasks,
				agents:        agentMgr,
				fetchReviewsFn: func() (github.ReviewSummary, error) {
					return github.ReviewSummary{}, tt.fetchErr
				},
				fetchPRStateFn: func(repo string, number int) (github.PRState, error) {
					reconcileCalled = true
					return github.PRState{State: "MERGED"}, nil
				},
			}

			r.pollAndMonitorPRs(context.Background())

			if reconcileCalled != tt.wantReconcile {
				t.Errorf("reconcileCalled = %v, want %v", reconcileCalled, tt.wantReconcile)
			}
		})
	}
}

func TestPollAndMonitorPRs_BudgetExhaustedUsesSingleRESTFallbackReconcile(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("Human review PR", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(77),
	}); err != nil {
		t.Fatal(err)
	}

	reconcileCalls := 0
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), emit: func(string, any) {}},
		tasks:         tasks,
		agents:        agentMgr,
		fetchReviewsFn: func() (github.ReviewSummary, error) {
			return github.ReviewSummary{}, github.ErrBudgetExhausted
		},
		fetchPRStateFn: func(repo string, number int) (github.PRState, error) {
			reconcileCalls++
			if repo != "o/r" || number != 77 {
				t.Fatalf("state fetch = %s#%d, want o/r#77", repo, number)
			}
			return github.PRState{State: "MERGED"}, nil
		},
	}

	r.pollAndMonitorPRs(context.Background())

	if reconcileCalls != 1 {
		t.Fatalf("reconcileCalls = %d, want 1", reconcileCalls)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusDone || got.Outcome != "merged" {
		t.Fatalf("status/outcome = %q/%q, want done/merged", got.Status, got.Outcome)
	}
}

func TestPollSecondaryReconcilesKnownTaskPRsWithoutSearch(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	openTask, err := tasks.Create("Ready PR", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(openTask.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(42),
	}); err != nil {
		t.Fatal(err)
	}

	closedTask, err := tasks.Create("Merged PR", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(closedTask.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(43),
	}); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())

	fetchReviewsCalled := false
	knownFetches := 0
	merged := false
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler), emit: func(string, any) {}},
		tasks:         tasks,
		projects:      projects,
		agents:        agentMgr,
		prTracker:     github.NewIssueTracker(0),
		cfg:           &config.Config{GitHub: config.GitHubConfig{PollerRole: "secondary"}},
		fetchReviewsFn: func() (github.ReviewSummary, error) {
			fetchReviewsCalled = true
			return github.ReviewSummary{}, nil
		},
		fetchKnownPRsFn: func(refs []github.PRRef) []github.MonitorPRResult {
			knownFetches++
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				results[i] = github.MonitorPRResult{Repo: ref.Repo, Number: ref.Number}
				if ref.Number != 42 {
					continue
				}
				results[i].Open = true
				results[i].PR = github.PullRequest{
					Number:          42,
					Repository:      ref.Repo,
					HeadSHA:         "abc123",
					Mergeable:       "MERGEABLE",
					CIStatus:        "SUCCESS",
					CopilotReviewed: true,
				}
			}
			return results
		},
		fetchPRStateFn: func(repo string, number int) (github.PRState, error) {
			if repo != "o/r" {
				t.Fatalf("repo = %q, want o/r", repo)
			}
			switch number {
			case 42:
				return github.PRState{State: "OPEN"}, nil
			case 43:
				return github.PRState{State: "MERGED"}, nil
			default:
				t.Fatalf("unexpected PR number %d", number)
				return github.PRState{}, nil
			}
		},
		mergePR: func(repo string, number int) error {
			if repo != "o/r" || number != 42 {
				t.Fatalf("merge target = %s#%d, want o/r#42", repo, number)
			}
			merged = true
			return nil
		},
	}

	r.Poll(t.Context())

	if fetchReviewsCalled {
		t.Fatal("secondary poller called FetchReviews search path")
	}
	if knownFetches != 1 {
		t.Fatalf("knownFetches = %d, want 1", knownFetches)
	}
	if !merged {
		t.Fatal("secondary poller did not act on ready linked PR")
	}
	got, err := tasks.Get(closedTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusDone || got.Outcome != "merged" {
		t.Fatalf("closed linked PR status/outcome = %q/%q, want done/merged", got.Status, got.Outcome)
	}
}
