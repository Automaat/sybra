package sybra

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// TestFetchKnownTaskPRs_SkipsFullFetchForKnownReadyPR verifies that once a PR
// has been observed ready-to-merge (REST-sourced clean+approved+green), a
// later poll cycle for the same, unmoved head SHA reuses the cached snapshot
// instead of issuing another full per-PR fetch.
func TestFetchKnownTaskPRs_SkipsFullFetchForKnownReadyPR(t *testing.T) {
	readyPR := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 50, HeadSHA: "sha50",
		Mergeable:      "MERGEABLE",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: true,
	}

	fullFetches := 0
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		fetchKnownPRsFn: func(refs []github.PRRef) []github.MonitorPRResult {
			fullFetches++
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				results[i] = github.MonitorPRResult{Repo: ref.Repo, Number: ref.Number, Open: true, PR: readyPR}
			}
			return results
		},
		fetchHeadStateFn: func(repo string, number int) (string, bool, error) {
			return "sha50", true, nil
		},
	}

	matchers := []github.TaskMatcher{{ID: "t1", PRNumber: 50, ProjectID: "pet-owner/pet-repo"}}

	first := r.fetchKnownTaskPRs(matchers)
	if len(first) != 1 || first[0].Number != 50 {
		t.Fatalf("first cycle: got %+v, want [readyPR]", first)
	}
	if fullFetches != 1 {
		t.Fatalf("full fetches after first cycle = %d, want 1", fullFetches)
	}

	second := r.fetchKnownTaskPRs(matchers)
	if len(second) != 1 || second[0].Number != 50 {
		t.Fatalf("second cycle: got %+v, want [readyPR]", second)
	}
	if fullFetches != 1 {
		t.Fatalf("full fetches after second cycle = %d, want still 1 (cache should have skipped it)", fullFetches)
	}
}

// TestFetchKnownTaskPRs_ForcePushInvalidatesReadyCache verifies that a head
// SHA change (force-push) between cycles evicts the cached ready-state and
// forces a full re-fetch instead of trusting the stale approval/CI verdict.
func TestFetchKnownTaskPRs_ForcePushInvalidatesReadyCache(t *testing.T) {
	readyPR := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 50, HeadSHA: "sha50",
		Mergeable:      "MERGEABLE",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: true,
	}
	// After the force-push, the new head is no longer approved/clean.
	forcedPushedPR := readyPR
	forcedPushedPR.HeadSHA = "sha-forced"
	forcedPushedPR.RESTApproved = false

	fullFetches := 0
	currentHeadSHA := "sha50"
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		fetchKnownPRsFn: func(refs []github.PRRef) []github.MonitorPRResult {
			fullFetches++
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				pr := readyPR
				if currentHeadSHA == "sha-forced" {
					pr = forcedPushedPR
				}
				results[i] = github.MonitorPRResult{Repo: ref.Repo, Number: ref.Number, Open: true, PR: pr}
			}
			return results
		},
		fetchHeadStateFn: func(repo string, number int) (string, bool, error) {
			return currentHeadSHA, true, nil
		},
	}

	matchers := []github.TaskMatcher{{ID: "t1", PRNumber: 50, ProjectID: "pet-owner/pet-repo"}}

	if got := r.fetchKnownTaskPRs(matchers); len(got) != 1 || got[0].HeadSHA != "sha50" {
		t.Fatalf("first cycle: got %+v", got)
	}
	if fullFetches != 1 {
		t.Fatalf("full fetches after first cycle = %d, want 1", fullFetches)
	}

	// Simulate a force-push: the head SHA moves and the cheap probe observes it.
	currentHeadSHA = "sha-forced"

	got := r.fetchKnownTaskPRs(matchers)
	if len(got) != 1 || got[0].HeadSHA != "sha-forced" {
		t.Fatalf("second cycle: got %+v, want fresh forced-push snapshot", got)
	}
	if fullFetches != 2 {
		t.Fatalf("full fetches after second cycle = %d, want 2 (force-push must invalidate the cache)", fullFetches)
	}
	if got[0].RESTApproved {
		t.Fatal("stale approval must not survive a force-push")
	}

	// A third cycle at the same (unmoved) new head SHA should again skip the
	// full fetch — but only if the new state was itself ready; here it isn't
	// (RESTApproved=false), so the cache should stay empty and every
	// subsequent cycle keeps re-fetching until re-approved.
	third := r.fetchKnownTaskPRs(matchers)
	if fullFetches != 3 {
		t.Fatalf("full fetches after third cycle = %d, want 3 (not-ready state must never be cached)", fullFetches)
	}
	if len(third) != 1 {
		t.Fatalf("third cycle: got %+v", third)
	}
}

// TestFetchKnownTaskPRs_ClosedAtSameHeadInvalidatesReadyCache verifies that a
// PR merged or closed externally (e.g. by a human outside Sybra) at the exact
// same head SHA it had while cached as ready is not replayed as still open.
// A head-SHA-only probe cannot distinguish this from "still open and ready"
// since merging/closing a PR does not change its head commit — the probe
// must also check state, or advanceClosedTaskPRs would never see the PR as
// closed and the task would be stuck in review forever.
func TestFetchKnownTaskPRs_ClosedAtSameHeadInvalidatesReadyCache(t *testing.T) {
	readyPR := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 50, HeadSHA: "sha50",
		Mergeable:      "MERGEABLE",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: true,
	}

	fullFetches := 0
	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		fetchKnownPRsFn: func(refs []github.PRRef) []github.MonitorPRResult {
			fullFetches++
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				// The PR was merged externally: same head SHA, but no longer open.
				results[i] = github.MonitorPRResult{Repo: ref.Repo, Number: ref.Number, Open: false, PR: readyPR}
			}
			return results
		},
		// The cheap probe reports the same head SHA but a closed state — this is
		// exactly what `gh pr view` returns for a PR merged/closed without a
		// subsequent push.
		fetchHeadStateFn: func(repo string, number int) (string, bool, error) {
			return "sha50", false, nil
		},
		readyPRCache: map[string]readyPRState{
			"pet-owner/pet-repo#50": {HeadSHA: "sha50", PR: readyPR},
		},
	}

	matchers := []github.TaskMatcher{{ID: "t1", PRNumber: 50, ProjectID: "pet-owner/pet-repo"}}

	got := r.fetchKnownTaskPRs(matchers)
	if len(got) != 0 {
		t.Fatalf("got %+v, want no open PRs (the cached PR is now closed)", got)
	}
	if fullFetches != 1 {
		t.Fatalf("full fetches = %d, want 1 (closed state must force a full fetch, not reuse the cache)", fullFetches)
	}
	if _, ok := r.readyPRCache["pet-owner/pet-repo#50"]; ok {
		t.Fatal("readyPRCache entry must be evicted once the cheap probe reports the PR is no longer open")
	}
}

// TestHandleAutoMerge_EvictsReadyCache verifies that a successful merge
// evicts the PR's readyPRCache entry, so the next poll cycle does a fresh
// fetch instead of replaying a stale "still open and ready" snapshot for a
// PR that is now actually merged and closed.
func TestHandleAutoMerge_EvictsReadyCache(t *testing.T) {
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
	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		PRNumber:  task.Ptr(50),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	pr := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 50, HeadSHA: "sha50",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: true,
	}

	r := &ReviewHandler{
		DomainHandler: DomainHandler{logger: slog.New(slog.DiscardHandler)},
		tasks:         tasks,
		projects:      projStore,
		prTracker:     github.NewIssueTracker(0),
		mergePRViaREST: func(repo string, number int, headSHA string) error {
			return nil
		},
		readyPRCache: map[string]readyPRState{
			"pet-owner/pet-repo#50": {HeadSHA: "sha50", PR: pr},
		},
	}

	r.handleAutoMerge(github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})

	if _, ok := r.readyPRCache["pet-owner/pet-repo#50"]; ok {
		t.Fatal("readyPRCache entry must be evicted after a merge attempt")
	}
}
