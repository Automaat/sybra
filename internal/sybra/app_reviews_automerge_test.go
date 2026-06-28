package sybra

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func TestReadyForCopilotAutoMerge(t *testing.T) {
	t.Parallel()
	// A clean, green PR that Copilot has reviewed with no open threads.
	base := github.PullRequest{
		Mergeable:       "MERGEABLE",
		CIStatus:        "SUCCESS",
		CopilotReviewed: true,
		UnresolvedCount: 0,
		ReviewDecision:  "",
	}
	withDraft := base
	withDraft.IsDraft = true
	noCopilot := base
	noCopilot.CopilotReviewed = false
	unresolved := base
	unresolved.UnresolvedCount = 2
	changes := base
	changes.ReviewDecision = "CHANGES_REQUESTED"
	conflict := base
	conflict.Mergeable = "CONFLICTING"
	ciFail := base
	ciFail.CIStatus = "FAILURE"
	ciPending := base
	ciPending.CIStatus = "PENDING"
	noChecks := base
	noChecks.CIStatus = ""
	approved := base
	approved.ReviewDecision = "APPROVED"

	tests := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"copilot reviewed, clean, green", base, true},
		{"no checks counts as green", noChecks, true},
		{"human approved also fine", approved, true},
		{"draft blocks", withDraft, false},
		{"copilot not reviewed blocks", noCopilot, false},
		{"unresolved threads block", unresolved, false},
		{"changes requested blocks", changes, false},
		{"conflict blocks", conflict, false},
		{"ci failure blocks", ciFail, false},
		{"ci pending blocks", ciPending, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := readyForCopilotAutoMerge(tt.pr); got != tt.want {
				t.Errorf("readyForCopilotAutoMerge() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHandleAutoMerge_GatesOnCopilot verifies the pet auto-merge path only
// merges when Copilot has reviewed and the PR is otherwise clean, and never
// merges a non-pet PR.
func TestHandleAutoMerge_GatesOnCopilot(t *testing.T) {
	tests := []struct {
		name       string
		projectID  string
		tags       []string
		pr         github.PullRequest
		wantMerged bool
	}{
		{
			name:      "pet, copilot reviewed -> merges",
			projectID: "pet-owner/pet-repo",
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 11,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true,
			},
			wantMerged: true,
		},
		{
			name:      "pet renovate-fix, no copilot -> merges (bypass)",
			projectID: "pet-owner/pet-repo",
			tags:      []string{"renovate-fix"},
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 15,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false,
			},
			wantMerged: true,
		},
		{
			name:      "pet, copilot not reviewed -> holds",
			projectID: "pet-owner/pet-repo",
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 12,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false,
			},
			wantMerged: false,
		},
		{
			name:      "pet, unresolved threads -> holds",
			projectID: "pet-owner/pet-repo",
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 13,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true, UnresolvedCount: 1,
			},
			wantMerged: false,
		},
		{
			name:      "work, copilot reviewed -> never auto-merges",
			projectID: "work-owner/work-repo",
			pr: github.PullRequest{
				Repository: "work-owner/work-repo", Number: 14,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true,
			},
			wantMerged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projDir := t.TempDir()
			projStore, err := project.NewStore(projDir, t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			mustWriteProjectYAML(t, projDir, "pet-owner/pet-repo", project.ProjectTypePet)
			mustWriteProjectYAML(t, projDir, "work-owner/work-repo", project.ProjectTypeWork)

			taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
			if err != nil {
				t.Fatalf("task NewStore: %v", err)
			}
			tasks := task.NewManager(taskStore, nil)
			created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			upd := task.Update{
				Status:    task.Ptr(task.StatusInReview),
				PRNumber:  task.Ptr(tt.pr.Number),
				ProjectID: task.Ptr(tt.projectID),
			}
			if tt.tags != nil {
				upd.Tags = &tt.tags
			}
			if _, err := tasks.Update(created.ID, upd); err != nil {
				t.Fatalf("update: %v", err)
			}

			var mergedRepo string
			var mergedNum int
			r := &ReviewHandler{
				DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
				tasks:         tasks,
				projects:      projStore,
				prTracker:     github.NewIssueTracker(time.Minute),
				mergePR: func(repo string, number int) error {
					mergedRepo, mergedNum = repo, number
					return nil
				},
			}

			r.handleAutoMerge(github.PRIssue{
				Kind:   github.PRIssueReadyToMerge,
				TaskID: created.ID,
				PR:     tt.pr,
			})

			merged := mergedNum != 0
			if merged != tt.wantMerged {
				t.Fatalf("merged=%v (repo=%q num=%d), want merged=%v", merged, mergedRepo, mergedNum, tt.wantMerged)
			}
		})
	}
}

func TestBlockedOnlyByThreads(t *testing.T) {
	t.Parallel()
	base := github.PullRequest{
		Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true,
		ReviewDecision: "", UnresolvedCount: 1,
	}
	noThreads := base
	noThreads.UnresolvedCount = 0
	noCopilot := base
	noCopilot.CopilotReviewed = false
	changes := base
	changes.ReviewDecision = "CHANGES_REQUESTED"
	ciFail := base
	ciFail.CIStatus = "FAILURE"

	tests := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"blocked only by threads", base, true},
		{"no unresolved threads -> not blocked-by-threads", noThreads, false},
		{"no copilot review -> not eligible", noCopilot, false},
		{"changes requested -> not eligible", changes, false},
		{"ci failure -> not eligible", ciFail, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := blockedOnlyByThreads(tt.pr); got != tt.want {
				t.Errorf("blockedOnlyByThreads() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveAddressedCopilotThreads(t *testing.T) {
	projDir := t.TempDir()
	projStore, err := project.NewStore(projDir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustWriteProjectYAML(t, projDir, "pet-owner/pet-repo", project.ProjectTypePet)

	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatalf("task NewStore: %v", err)
	}
	tasks := task.NewManager(taskStore, nil)
	created, err := tasks.Create("ship", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		PRNumber:  task.Ptr(21),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// T1: addressed Copilot thread (outdated) -> resolve. T2: live Copilot thread
	// -> skip. T3: human thread (even outdated) -> skip. T4: already resolved ->
	// skip. T5: Copilot thread the agent replied to (not outdated) -> resolve.
	// T6: Copilot thread, no reply yet (Copilot still last) -> skip.
	threads := []github.ReviewThread{
		{ID: "T1", AuthorLogin: "copilot-pull-request-reviewer[bot]", IsOutdated: true},
		{ID: "T2", AuthorLogin: "Copilot", IsOutdated: false},
		{ID: "T3", AuthorLogin: "dev", IsOutdated: true},
		{ID: "T4", AuthorLogin: "Copilot", IsOutdated: true, IsResolved: true},
		{ID: "T5", AuthorLogin: "Copilot", IsOutdated: false, LastAuthorLogin: "dev"},
		{ID: "T6", AuthorLogin: "Copilot", IsOutdated: false, LastAuthorLogin: "Copilot"},
	}
	var resolvedIDs []string
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
		projects:      projStore,
		agents:        agents,
		fetchThreads: func(repo string, number int) ([]github.ReviewThread, error) {
			if repo != "pet-owner/pet-repo" || number != 21 {
				t.Errorf("fetchThreads(%q,%d) unexpected", repo, number)
			}
			return threads, nil
		},
		resolveThread: func(id string) error {
			resolvedIDs = append(resolvedIDs, id)
			return nil
		},
		// The agent posts as "dev"; T5's last reply is "dev" → addressed.
		viewerLoginFn: func() string { return "dev" },
	}

	prs := []github.PullRequest{{
		Number: 21, Repository: "pet-owner/pet-repo", HeadRefName: "feat",
		Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true, UnresolvedCount: 3,
	}}
	r.resolveAddressedCopilotThreads(all, prs)

	if len(resolvedIDs) != 2 || resolvedIDs[0] != "T1" || resolvedIDs[1] != "T5" {
		t.Fatalf("resolvedIDs = %v, want [T1 T5]", resolvedIDs)
	}
}

// TestResolveCopilotThreads_humanReplyNotDismissed locks the fix for the
// over-broad agentReplied predicate: a human collaborator replying on a Copilot
// thread must NOT auto-resolve it (that would discard live feedback). Only the
// agent's own identity counts as "addressed".
func TestResolveCopilotThreads_humanReplyNotDismissed(t *testing.T) {
	threads := []github.ReviewThread{
		// Copilot thread, last reply by a human collaborator (not the agent).
		{ID: "H1", AuthorLogin: "Copilot", IsOutdated: false, LastAuthorLogin: "alice"},
		// Copilot thread the agent itself replied to → addressed.
		{ID: "A1", AuthorLogin: "Copilot", IsOutdated: false, LastAuthorLogin: "agent-bot"},
	}
	var resolvedIDs []string
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		fetchThreads:  func(string, int) ([]github.ReviewThread, error) { return threads, nil },
		resolveThread: func(id string) error { resolvedIDs = append(resolvedIDs, id); return nil },
	}
	pr := github.PullRequest{Number: 7, Repository: "o/r"}

	r.resolveCopilotThreadsForPR("task1", pr, "agent-bot")

	if len(resolvedIDs) != 1 || resolvedIDs[0] != "A1" {
		t.Fatalf("resolvedIDs = %v, want [A1] (human reply on H1 must be left alone)", resolvedIDs)
	}
}

// TestResolveCopilotThreads_emptyAgentLoginFallsBackToAuthor locks the fix for
// the empty-viewer edge: when ViewerLogin() fails (agentLogin ""), the PR author
// stands in for the agent's identity, so an addressed thread is still resolved
// rather than re-parking the pet PR.
func TestResolveCopilotThreads_emptyAgentLoginFallsBackToAuthor(t *testing.T) {
	threads := []github.ReviewThread{
		// Copilot thread the agent (== PR author "me") replied to → addressed.
		{ID: "A1", AuthorLogin: "Copilot", IsOutdated: false, LastAuthorLogin: "me"},
	}
	var resolvedIDs []string
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		fetchThreads:  func(string, int) ([]github.ReviewThread, error) { return threads, nil },
		resolveThread: func(id string) error { resolvedIDs = append(resolvedIDs, id); return nil },
	}
	pr := github.PullRequest{Number: 7, Repository: "o/r", Author: "me"}

	r.resolveCopilotThreadsForPR("task1", pr, "")

	if len(resolvedIDs) != 1 || resolvedIDs[0] != "A1" {
		t.Fatalf("resolvedIDs = %v, want [A1] (empty agentLogin must fall back to PR author)", resolvedIDs)
	}
}

// TestEscalateExhaustedFix locks the scope of escalation: every fixable kind
// (conflict, ci_failure, comments) parks a task to human-required once its
// durable retry budget is spent — leaving a capped kind un-escalated would
// strand it (capped, never retried, never surfaced). ready_to_merge never
// escalates, and an already-parked task is left untouched.
func TestEscalateExhaustedFix(t *testing.T) {
	newHandler := func(t *testing.T) (*ReviewHandler, *task.Manager, string) {
		t.Helper()
		store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		tasks := task.NewManager(store, nil)
		created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := tasks.Update(created.ID, task.Update{
			Status:   task.Ptr(task.StatusInReview),
			PRNumber: task.Ptr(9),
		}); err != nil {
			t.Fatalf("update: %v", err)
		}
		r := &ReviewHandler{
			DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
			tasks:         tasks,
			prTracker:     github.NewIssueTracker(30 * time.Minute),
		}
		return r, tasks, created.ID
	}

	t.Run("conflict exhaustion parks to human-required", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueConflict, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusHumanRequired {
			t.Fatalf("conflict: status = %q, want human-required", got.Status)
		}
	})

	t.Run("ci_failure exhaustion parks to human-required", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueCIFailure, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusHumanRequired {
			t.Fatalf("ci_failure: status = %q, want human-required", got.Status)
		}
	})

	t.Run("ready_to_merge never escalates", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusInReview {
			t.Fatalf("ready_to_merge: status = %q, want in-review (no escalation)", got.Status)
		}
	})

	t.Run("comments exhaustion parks to human-required and clears tracker", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		// Spend the budget so the tracker has a non-zero retry count to clear.
		for range github.MaxRetries {
			r.prTracker.MarkHandled(id, github.PRIssueComments, "sha")
		}
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueComments, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusHumanRequired {
			t.Fatalf("comments: status = %q, want human-required", got.Status)
		}
		if got.StatusReason == "" {
			t.Error("comments: want a status reason explaining the escalation")
		}
		if n := r.prTracker.Retries(id, github.PRIssueComments); n != 0 {
			t.Errorf("tracker retries = %d after escalation, want 0 (cleared for a human un-park)", n)
		}
	})

	t.Run("already human-required is left untouched", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		if _, err := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusHumanRequired),
			StatusReason: task.Ptr("set by a human"),
		}); err != nil {
			t.Fatalf("pre-set: %v", err)
		}
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueComments, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.StatusReason != "set by a human" {
			t.Errorf("status reason overwritten = %q, want idempotent no-op", got.StatusReason)
		}
	})
}
