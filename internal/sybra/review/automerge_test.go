package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
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
	sourcedViaREST := base
	sourcedViaREST.SourcedViaREST = true

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
		{"REST-sourced PR always blocks, even when otherwise green", sourcedViaREST, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NewMergeGate(tt.pr).ReadyForMerge(MergePolicyCopilot, false); got != tt.want {
				t.Errorf("ReadyForMerge(Copilot) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadyForRESTAutoMerge(t *testing.T) {
	t.Parallel()
	base := github.PullRequest{
		SourcedViaREST:     true,
		RESTMergeableState: "clean",
		RESTCIFetched:      true,
		CIStatus:           "SUCCESS",
		RESTApproved:       true,
	}
	withDraft := base
	withDraft.IsDraft = true
	noChecks := base
	noChecks.CIStatus = ""
	notApproved := base
	notApproved.RESTApproved = false
	ciFetchFailed := base
	ciFetchFailed.RESTCIFetched = false
	ciFail := base
	ciFail.CIStatus = "FAILURE"

	tests := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"clean, fetched, green, approved", base, true},
		{"no checks counts as green", noChecks, true},
		{"draft blocks", withDraft, false},
		{"not approved blocks", notApproved, false},
		{"unfetched CI blocks even though CIStatus is empty", ciFetchFailed, false},
		{"ci failure blocks", ciFail, false},
	}
	for _, state := range []string{"blocked", "behind", "unstable", "unknown", ""} {
		pr := base
		pr.RESTMergeableState = state
		tests = append(tests, struct {
			name string
			pr   github.PullRequest
			want bool
		}{name: "mergeable_state=" + state + " blocks", pr: pr, want: false})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NewMergeGate(tt.pr).ReadyForMerge(MergePolicyCopilot, false); got != tt.want {
				t.Errorf("ReadyForMerge(Copilot) = %v, want %v", got, tt.want)
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
		reviewed   bool
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
			name:      "pet self-authored bot, no copilot -> merges (bypass)",
			projectID: "pet-owner/pet-repo",
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 16,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false, SelfAuthoredBot: true,
			},
			wantMerged: true,
		},
		{
			name:      "pet self-authored bot, changes requested -> holds",
			projectID: "pet-owner/pet-repo",
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 17,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false,
				SelfAuthoredBot: true, ReviewDecision: "CHANGES_REQUESTED",
			},
			wantMerged: false,
		},
		{
			name:      "pet, sybra-reviewed without copilot -> merges",
			projectID: "pet-owner/pet-repo",
			reviewed:  true,
			pr: github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 18,
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false,
			},
			wantMerged: true,
		},
		{
			name:      "pet, not sybra-reviewed and no copilot -> holds",
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
				Reviewed:  task.Ptr(tt.reviewed),
			}
			if tt.tags != nil {
				upd.Tags = &tt.tags
			}
			if _, err := tasks.Update(created.ID, upd); err != nil {
				t.Fatalf("update: %v", err)
			}

			var mergedRepo string
			var mergedNum int
			r := &Handler{
				logger:    slog.New(slog.DiscardHandler),
				tasks:     tasks,
				projects:  projStore,
				prTracker: github.NewIssueTracker(time.Minute),
				mergePR: func(repo string, number int) error {
					mergedRepo, mergedNum = repo, number
					return nil
				},
			}

			r.handleAutoMerge(context.Background(), github.PRIssue{
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

func TestReadyToArmNativeAutoMerge(t *testing.T) {
	t.Parallel()
	base := github.PullRequest{
		Mergeable:       "MERGEABLE",
		CIStatus:        "PENDING",
		CopilotReviewed: true,
		UnresolvedCount: 0,
		ReviewDecision:  "",
	}
	ciFail := base
	ciFail.CIStatus = "FAILURE"
	alreadyArmed := base
	alreadyArmed.AutoMergeEnabled = true
	notMergeable := base
	notMergeable.Mergeable = "CONFLICTING"
	renovate := base
	renovate.Author = "renovate[bot]"
	draft := base
	draft.IsDraft = true
	noCopilot := base
	noCopilot.CopilotReviewed = false
	unresolved := base
	unresolved.UnresolvedCount = 1
	changesRequested := base
	changesRequested.ReviewDecision = "CHANGES_REQUESTED"
	ciSuccess := base
	ciSuccess.CIStatus = "SUCCESS"

	tests := []struct {
		name string
		pr   github.PullRequest
		want bool
	}{
		{"all eligible (CI still pending)", base, true},
		{"CI already green also eligible", ciSuccess, true},
		{"CI FAILURE blocks", ciFail, false},
		{"already armed blocks", alreadyArmed, false},
		{"not mergeable blocks", notMergeable, false},
		{"renovate-fix (bot author) blocks", renovate, false},
		{"draft blocks", draft, false},
		{"no copilot review blocks", noCopilot, false},
		{"unresolved threads block", unresolved, false},
		{"changes requested blocks", changesRequested, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NewMergeGate(tt.pr).ReadyToArm(); got != tt.want {
				t.Errorf("ReadyToArm() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHandleAutoMerge_ArmsNative verifies handleAutoMerge prefers arming
// GitHub's native auto-merge over Sybra's own squash merge when the config
// flag is on, the project is pet, the base-branch capability check passes,
// and the PR is otherwise ready — and that it falls back to the legacy
// green-gated merge whenever any of those conditions doesn't hold.
func TestHandleAutoMerge_ArmsNative(t *testing.T) {
	tests := []struct {
		name           string
		nativeEnabled  bool
		projectType    project.ProjectType
		supportsNative bool
		wantArmed      bool
		wantMerged     bool
	}{
		{
			name:           "config on + pet + capability true + gate true -> arms native",
			nativeEnabled:  true,
			projectType:    project.ProjectTypePet,
			supportsNative: true,
			wantArmed:      true,
			wantMerged:     false,
		},
		{
			name:           "config off -> legacy merge",
			nativeEnabled:  false,
			projectType:    project.ProjectTypePet,
			supportsNative: true,
			wantArmed:      false,
			wantMerged:     true,
		},
		{
			name:           "work-typed project -> legacy merge",
			nativeEnabled:  true,
			projectType:    project.ProjectTypeWork,
			supportsNative: true,
			wantArmed:      false,
			wantMerged:     false, // handleAutoMerge never merges work-typed projects at all
		},
		{
			name:           "capability false -> legacy merge",
			nativeEnabled:  true,
			projectType:    project.ProjectTypePet,
			supportsNative: false,
			wantArmed:      false,
			wantMerged:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projDir := t.TempDir()
			projStore, err := project.NewStore(projDir, t.TempDir())
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			mustWriteProjectYAML(t, projDir, "pet-owner/pet-repo", tt.projectType)

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
				PRNumber:  task.Ptr(11),
				ProjectID: task.Ptr("pet-owner/pet-repo"),
			}); err != nil {
				t.Fatalf("update: %v", err)
			}

			pr := github.PullRequest{
				Repository: "pet-owner/pet-repo", Number: 11, BaseRefName: "main",
				Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: true,
			}

			var mergedRepo string
			var mergedNum int
			var armedRepo string
			var armedNum int
			r := &Handler{
				logger:    slog.New(slog.DiscardHandler),
				tasks:     tasks,
				projects:  projStore,
				prTracker: github.NewIssueTracker(time.Minute),
				cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: tt.nativeEnabled}},
				mergePR: func(repo string, number int) error {
					mergedRepo, mergedNum = repo, number
					return nil
				},
				supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
					if baseBranch != "main" {
						t.Errorf("supportsAutoMergeFn called with baseBranch = %q, want %q", baseBranch, "main")
					}
					return tt.supportsNative, nil
				},
				enableAutoMergeFn: func(repo string, number int) error {
					armedRepo, armedNum = repo, number
					return nil
				},
			}

			r.handleAutoMerge(context.Background(), github.PRIssue{
				Kind:   github.PRIssueReadyToMerge,
				TaskID: created.ID,
				PR:     pr,
			})

			armed := armedNum != 0
			merged := mergedNum != 0
			if armed != tt.wantArmed {
				t.Errorf("armed=%v (repo=%q num=%d), want armed=%v", armed, armedRepo, armedNum, tt.wantArmed)
			}
			if merged != tt.wantMerged {
				t.Errorf("merged=%v (repo=%q num=%d), want merged=%v", merged, mergedRepo, mergedNum, tt.wantMerged)
			}
		})
	}
}

func TestHandleAutoMerge_FallsBackToNativeWhenDirectMergeBlockedByPolicy(t *testing.T) {
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
		PRNumber:  task.Ptr(2385),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
		Reviewed:  task.Ptr(true),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	tracker := github.NewIssueTracker(time.Minute)
	var mergeCalled bool
	var armedRepo string
	var armedNum int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: tracker,
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: true}},
		mergePR: func(repo string, number int) error {
			mergeCalled = true
			return fmt.Errorf("gh pr merge %d: X Pull request owner/repo#%d is not mergeable: the base branch policy prohibits the merge.\nTo have the pull request merged after all the requirements have been met, add the `--auto` flag.: exit status 1", number, number)
		},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			if baseBranch != "main" {
				t.Errorf("supportsAutoMergeFn called with baseBranch = %q, want %q", baseBranch, "main")
			}
			return true, nil
		},
		enableAutoMergeFn: func(repo string, number int) error {
			armedRepo, armedNum = repo, number
			return nil
		},
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{
		Kind:   github.PRIssueReadyToMerge,
		TaskID: created.ID,
		PR: github.PullRequest{
			Repository:  "pet-owner/pet-repo",
			Number:      2385,
			BaseRefName: "main",
			HeadSHA:     "sha2385",
			Mergeable:   "MERGEABLE",
			CIStatus:    "SUCCESS",
		},
	})

	if !mergeCalled {
		t.Fatal("direct merge was not attempted before fallback")
	}
	if armedRepo != "pet-owner/pet-repo" || armedNum != 2385 {
		t.Fatalf("armed native auto-merge for %s#%d, want pet-owner/pet-repo#2385", armedRepo, armedNum)
	}
	if got := tracker.Retries(created.ID, github.PRIssueReadyToMerge); got != 1 {
		t.Fatalf("ready_to_merge retries = %d, want 1 after native handoff", got)
	}
}

func TestHandleAutoMerge_DirectMergePolicyBlockedHonorsNativeKillSwitch(t *testing.T) {
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
		PRNumber:  task.Ptr(2385),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
		Reviewed:  task.Ptr(true),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	tracker := github.NewIssueTracker(time.Minute)
	var mergeCalled bool
	var supportChecked bool
	var armed bool
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: tracker,
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: false}},
		mergePR: func(repo string, number int) error {
			mergeCalled = true
			return fmt.Errorf("gh pr merge %d: X Pull request owner/repo#%d is not mergeable: the base branch policy prohibits the merge.\nTo have the pull request merged after all the requirements have been met, add the `--auto` flag.: exit status 1", number, number)
		},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			supportChecked = true
			return true, nil
		},
		enableAutoMergeFn: func(repo string, number int) error {
			armed = true
			return nil
		},
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{
		Kind:   github.PRIssueReadyToMerge,
		TaskID: created.ID,
		PR: github.PullRequest{
			Repository:  "pet-owner/pet-repo",
			Number:      2385,
			BaseRefName: "main",
			HeadSHA:     "sha2385",
			Mergeable:   "MERGEABLE",
			CIStatus:    "SUCCESS",
		},
	})

	if !mergeCalled {
		t.Fatal("direct merge was not attempted")
	}
	if supportChecked {
		t.Fatal("native auto-merge support was checked while kill-switch was disabled")
	}
	if armed {
		t.Fatal("native auto-merge was armed while kill-switch was disabled")
	}
	if got := tracker.Retries(created.ID, github.PRIssueReadyToMerge); got != 0 {
		t.Fatalf("ready_to_merge retries = %d, want 0 when native auto-merge is disabled", got)
	}
}

func TestHandleAutoMerge_DirectMergeOrdinaryFailureDoesNotArmNative(t *testing.T) {
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
		PRNumber:  task.Ptr(42),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
		Reviewed:  task.Ptr(true),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var armed bool
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: true}},
		mergePR: func(repo string, number int) error {
			return fmt.Errorf("gh pr merge %d: transient network failure: exit status 1", number)
		},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			return true, nil
		},
		enableAutoMergeFn: func(repo string, number int) error {
			armed = true
			return nil
		},
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{
		Kind:   github.PRIssueReadyToMerge,
		TaskID: created.ID,
		PR: github.PullRequest{
			Repository:  "pet-owner/pet-repo",
			Number:      42,
			BaseRefName: "main",
			HeadSHA:     "sha42",
			Mergeable:   "MERGEABLE",
			CIStatus:    "SUCCESS",
		},
	})

	if armed {
		t.Fatal("ordinary direct merge failure armed native auto-merge")
	}
}

func TestHandleAutoMerge_REST(t *testing.T) {
	restApproved := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 30, HeadSHA: "sha30",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: true,
	}
	restNotApproved := restApproved
	restNotApproved.Number = 31
	restNotApproved.RESTApproved = false
	restCIFetchFailed := restApproved
	restCIFetchFailed.Number = 32
	restCIFetchFailed.RESTCIFetched = false
	restCIFetchFailed.CIStatus = ""
	restBlocked := restApproved
	restBlocked.Number = 33
	restBlocked.RESTMergeableState = "blocked"
	restCopilotCommentedOnly := restApproved
	restCopilotCommentedOnly.Number = 34
	restCopilotCommentedOnly.RESTApproved = false // Copilot COMMENTED never sets RESTApproved
	restRenovateGreenPR := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 35, HeadSHA: "sha35",
		SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
		CIStatus: "SUCCESS", RESTApproved: false,
	}
	nonRESTReadyIssue := github.PullRequest{
		Repository: "pet-owner/pet-repo", Number: 36,
		Mergeable: "MERGEABLE", CIStatus: "SUCCESS", CopilotReviewed: false,
	}

	tests := []struct {
		name       string
		tags       []string
		pr         github.PullRequest
		wantMerged bool
	}{
		{"REST-sourced, clean+fetched+green+approved -> merges via REST", nil, restApproved, true},
		{"REST-sourced, not approved -> holds", nil, restNotApproved, false},
		{"REST-sourced, CI fetch failed -> holds (empty CIStatus not read as green)", nil, restCIFetchFailed, false},
		{"REST-sourced, blocked mergeable_state -> holds", nil, restBlocked, false},
		{"REST-sourced, Copilot COMMENTED-only -> holds", nil, restCopilotCommentedOnly, false},
		{"REST-sourced renovate-fix, clean+fetched+green, no approval needed -> merges", []string{"renovate-fix"}, restRenovateGreenPR, true},
		{"non-REST-sourced ready issue in degraded handler -> holds (Copilot gate, not reviewed)", nil, nonRESTReadyIssue, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			upd := task.Update{
				Status:    task.Ptr(task.StatusInReview),
				PRNumber:  task.Ptr(tt.pr.Number),
				ProjectID: task.Ptr("pet-owner/pet-repo"),
			}
			if tt.tags != nil {
				upd.Tags = &tt.tags
			}
			if _, err := tasks.Update(created.ID, upd); err != nil {
				t.Fatalf("update: %v", err)
			}

			var restMergedRepo, restMergedSHA string
			var restMergedNum int
			var gqlMergeCalled bool
			r := &Handler{
				logger:    slog.New(slog.DiscardHandler),
				tasks:     tasks,
				projects:  projStore,
				prTracker: github.NewIssueTracker(time.Minute),
				mergePR: func(repo string, number int) error {
					gqlMergeCalled = true
					return nil
				},
				mergePRViaREST: func(repo string, number int, headSHA string) error {
					restMergedRepo, restMergedNum, restMergedSHA = repo, number, headSHA
					return nil
				},
			}

			r.handleAutoMerge(context.Background(), github.PRIssue{
				Kind:   github.PRIssueReadyToMerge,
				TaskID: created.ID,
				PR:     tt.pr,
			})

			merged := restMergedNum != 0
			if merged != tt.wantMerged {
				t.Fatalf("merged=%v (repo=%q num=%d), want merged=%v", merged, restMergedRepo, restMergedNum, tt.wantMerged)
			}
			if gqlMergeCalled {
				t.Error("a REST-sourced PR must never merge via the GraphQL gh-pr-merge path")
			}
			if merged && restMergedSHA != tt.pr.HeadSHA {
				t.Errorf("mergePRViaREST head sha = %q, want %q", restMergedSHA, tt.pr.HeadSHA)
			}
		})
	}
}

// TestHandleAutoMerge_REST_AuditPayload verifies a REST-sourced merge stamps
// sourced_via_rest, gate_evidence, and head_sha into the auto-merge audit
// event, while a GraphQL-sourced merge's payload stays free of those REST-only
// keys.
func TestHandleAutoMerge_REST_AuditPayload(t *testing.T) {
	tmp := t.TempDir()
	auditDir := filepath.Join(tmp, "audit")
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	defer auditLog.Close()

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
		PRNumber:  task.Ptr(40),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	r := &Handler{
		logger: slog.New(slog.DiscardHandler), audit: auditLog,
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		mergePRViaREST: func(repo string, number int, headSHA string) error {
			return nil
		},
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{
		Kind:   github.PRIssueReadyToMerge,
		TaskID: created.ID,
		PR: github.PullRequest{
			Repository: "pet-owner/pet-repo", Number: 40, HeadSHA: "sha40",
			SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
			CIStatus: "SUCCESS", RESTApproved: true,
		},
	})

	events := readExperienceAuditEvents(t, auditDir)
	var merged *audit.Event
	for i := range events {
		if events[i].Type == audit.EventPRAutoMerged {
			merged = &events[i]
		}
	}
	if merged == nil {
		t.Fatalf("no %s audit event; events=%+v", audit.EventPRAutoMerged, events)
		return
	}
	if merged.Data["sourced_via_rest"] != true {
		t.Errorf("sourced_via_rest = %v, want true", merged.Data["sourced_via_rest"])
	}
	if merged.Data["gate_evidence"] != "approved" {
		t.Errorf("gate_evidence = %v, want approved", merged.Data["gate_evidence"])
	}
	if merged.Data["head_sha"] != "sha40" {
		t.Errorf("head_sha = %v, want sha40", merged.Data["head_sha"])
	}
}

func TestHandleAutoMerge_FiresAppliedHook(t *testing.T) {
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
		PRNumber:  task.Ptr(41),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	hookCalls := 0
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		mergePR: func(repo string, number int) error {
			return nil
		},
		onAutoMergeApplied: func() {
			hookCalls++
		},
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{
		Kind:   github.PRIssueReadyToMerge,
		TaskID: created.ID,
		PR: github.PullRequest{
			Repository:      "pet-owner/pet-repo",
			Number:          41,
			Mergeable:       "MERGEABLE",
			CIStatus:        "SUCCESS",
			CopilotReviewed: true,
		},
	})

	if hookCalls != 1 {
		t.Fatalf("hookCalls = %d, want 1", hookCalls)
	}
}

// TestHandleAutoMerge_BacksOffRepeatedDirectMergeFailures locks in the
// #2450 fix: a direct-merge failure against an unchanged PR (same head SHA)
// must not be retried on every poll tick. Polling the same blocked PR
// repeatedly should attempt the merge exactly once and suppress every
// subsequent call, while a genuinely new push (head SHA change) reprobes
// immediately.
func TestHandleAutoMerge_BacksOffRepeatedDirectMergeFailures(t *testing.T) {
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
		PRNumber:  task.Ptr(77),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var mergeCalls int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		mergePR: func(repo string, number int) error {
			mergeCalls++
			return fmt.Errorf("gh pr merge %d: required status check \"ci\" is expected: exit status 1", number)
		},
	}

	pr := github.PullRequest{
		Repository:      "pet-owner/pet-repo",
		Number:          77,
		Mergeable:       "MERGEABLE",
		CIStatus:        "SUCCESS",
		CopilotReviewed: true,
		HeadSHA:         "sha-a",
	}

	for i := range 5 {
		r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
		if mergeCalls != 1 {
			t.Fatalf("after poll %d: mergeCalls = %d, want 1 (repeated polls against unchanged state must be suppressed)", i+1, mergeCalls)
		}
	}

	// A new push (head SHA change) must reprobe immediately, not wait out the
	// backoff window computed for the old SHA.
	pr.HeadSHA = "sha-b"
	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if mergeCalls != 2 {
		t.Fatalf("after head SHA change: mergeCalls = %d, want 2 (new push must reprobe immediately)", mergeCalls)
	}
}

// TestHandleAutoMerge_BackoffClearsOnSuccess verifies a merge that succeeds
// after prior failures against the same head SHA is not left backed off —
// Clear must run on the success path too, not only on arm.
func TestHandleAutoMerge_BackoffClearsOnSuccess(t *testing.T) {
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
		PRNumber:  task.Ptr(78),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	failNext := true
	var mergeCalls int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		mergePR: func(repo string, number int) error {
			mergeCalls++
			if failNext {
				return errors.New("gh pr merge: unexpected server error")
			}
			return nil
		},
	}

	pr := github.PullRequest{
		Repository:      "pet-owner/pet-repo",
		Number:          78,
		Mergeable:       "MERGEABLE",
		CIStatus:        "SUCCESS",
		CopilotReviewed: true,
		HeadSHA:         "sha-x",
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if mergeCalls != 1 {
		t.Fatalf("mergeCalls after first failure = %d, want 1", mergeCalls)
	}
	if got := r.mergeBackoff().Attempts(pr.Repository, pr.Number); got != 1 {
		t.Fatalf("backoff attempts after failure = %d, want 1", got)
	}

	// Same head SHA, still within the backoff window: a retry must not merge.
	failNext = false
	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if mergeCalls != 1 {
		t.Fatalf("mergeCalls while suppressed = %d, want 1", mergeCalls)
	}

	// A new push clears the suppression; the (now-succeeding) merge clears
	// the backoff entry entirely.
	pr.HeadSHA = "sha-y"
	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if mergeCalls != 2 {
		t.Fatalf("mergeCalls after new push = %d, want 2", mergeCalls)
	}
	if got := r.mergeBackoff().Attempts(pr.Repository, pr.Number); got != 0 {
		t.Fatalf("backoff attempts after success = %d, want 0 (cleared)", got)
	}
}

// TestHandleAutoMerge_ArmNotSupportedFallsThroughToDirectMerge locks in that
// a benign "native auto-merge unsupported" result (no error, just a
// repo/branch that doesn't offer it) must never be treated as a
// backoff-worthy failure — it would otherwise wrongly suppress the very same
// cycle's direct-merge fallback attempt.
func TestHandleAutoMerge_ArmNotSupportedFallsThroughToDirectMerge(t *testing.T) {
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
		PRNumber:  task.Ptr(79),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var mergedNum int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: true}},
		mergePR: func(repo string, number int) error {
			mergedNum = number
			return nil
		},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			return false, nil // repo/branch doesn't support native auto-merge
		},
	}

	pr := github.PullRequest{
		Repository:      "pet-owner/pet-repo",
		Number:          79,
		BaseRefName:     "main",
		Mergeable:       "MERGEABLE",
		CIStatus:        "SUCCESS",
		CopilotReviewed: true,
		HeadSHA:         "sha-z",
	}

	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if mergedNum != 79 {
		t.Fatalf("mergedNum = %d, want 79 (arm-unsupported must fall through to direct merge in the same cycle)", mergedNum)
	}
	if got := r.mergeBackoff().Attempts(pr.Repository, pr.Number); got != 0 {
		t.Fatalf("backoff attempts = %d, want 0 (unsupported is not a failure)", got)
	}
}

// TestHandleAutoMerge_ArmFailureBacksOff verifies a genuine arm error
// (EnableAutoMerge itself failing, not just an unsupported repo/branch) is
// backed off the same way a direct-merge failure is: repeated polls against
// the same head SHA attempt to arm exactly once, and a new push reprobes
// immediately.
func TestHandleAutoMerge_ArmFailureBacksOff(t *testing.T) {
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
		PRNumber:  task.Ptr(80),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var armCalls int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: true}},
		mergePR: func(repo string, number int) error {
			t.Fatal("direct merge must not be attempted while CI is still pending (arm-only eligible)")
			return nil
		},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			return true, nil
		},
		enableAutoMergeFn: func(repo string, number int) error {
			armCalls++
			return errors.New("gh pr merge --auto: unexpected server error")
		},
	}

	// CI still pending: eligible to arm, not eligible for the direct-merge
	// gate (ReadyForMerge(Copilot) requires CI green).
	pr := github.PullRequest{
		Repository:      "pet-owner/pet-repo",
		Number:          80,
		BaseRefName:     "main",
		Mergeable:       "MERGEABLE",
		CIStatus:        "PENDING",
		CopilotReviewed: true,
		HeadSHA:         "sha-arm-a",
	}

	for i := range 5 {
		r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
		if armCalls != 1 {
			t.Fatalf("after poll %d: armCalls = %d, want 1 (repeated polls against unchanged state must be suppressed)", i+1, armCalls)
		}
	}

	pr.HeadSHA = "sha-arm-b"
	r.handleAutoMerge(context.Background(), github.PRIssue{Kind: github.PRIssueReadyToMerge, TaskID: created.ID, PR: pr})
	if armCalls != 2 {
		t.Fatalf("after head SHA change: armCalls = %d, want 2 (new push must reprobe immediately)", armCalls)
	}
}

func TestMaybeArmNativeAutoMerge_BacksOffRepeatedFailuresUntilStateChanges(t *testing.T) {
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
		PRNumber:  task.Ptr(81),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	taskList, err := tasks.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var armCalls int
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{NativeAutoMerge: true}},
		supportsAutoMergeFn: func(repo, baseBranch string) (bool, error) {
			return true, nil
		},
		enableAutoMergeFn: func(repo string, number int) error {
			armCalls++
			return errors.New("gh pr merge --auto: API rate limit exceeded for user")
		},
	}

	pr := github.PullRequest{
		Repository:      "pet-owner/pet-repo",
		Number:          81,
		BaseRefName:     "main",
		Mergeable:       "MERGEABLE",
		CIStatus:        "PENDING",
		CopilotReviewed: true,
		HeadSHA:         "sha-arm-a",
	}

	for i := range 5 {
		r.maybeArmNativeAutoMerge(context.Background(), taskList, []github.PullRequest{pr}, nil)
		if armCalls != 1 {
			t.Fatalf("after poll %d: armCalls = %d, want 1 (repeated polls against unchanged state must be suppressed)", i+1, armCalls)
		}
	}

	pr.CIStatus = "SUCCESS"
	r.maybeArmNativeAutoMerge(context.Background(), taskList, []github.PullRequest{pr}, nil)
	if armCalls != 2 {
		t.Fatalf("after CI state change: armCalls = %d, want 2 (same-SHA state change must reprobe immediately)", armCalls)
	}
}

// TestHandleKnownPRConflictsViaREST_RoutesReadyToMerge verifies the
// budget-exhausted REST-only pass now routes a ready_to_merge issue through
// to handleAutoMerge (and its REST merge), where it used to be dropped
// alongside comments.
func TestHandleKnownPRConflictsViaREST_RoutesReadyToMerge(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	tk, err := tasks.Create("Ready PR", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		ProjectID: task.Ptr("pet-owner/pet-repo"),
		PRNumber:  task.Ptr(50),
	}); err != nil {
		t.Fatal(err)
	}

	projDir := t.TempDir()
	projStore, err := project.NewStore(projDir, t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mustWriteProjectYAML(t, projDir, "pet-owner/pet-repo", project.ProjectTypePet)

	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, slog.New(slog.DiscardHandler), t.TempDir())

	var restMergedNum int
	var gqlMergeCalled bool
	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		projects:  projStore,
		agents:    agentMgr,
		prTracker: github.NewIssueTracker(time.Minute),
		mergePR: func(repo string, number int) error {
			gqlMergeCalled = true
			return nil
		},
		mergePRViaREST: func(repo string, number int, headSHA string) error {
			restMergedNum = number
			return nil
		},
		fetchKnownPRsFn: func(refs []github.PRRef) []github.MonitorPRResult {
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				results[i] = github.MonitorPRResult{
					Repo: ref.Repo, Number: ref.Number, Open: true,
					PR: github.PullRequest{
						Number: ref.Number, Repository: ref.Repo, HeadSHA: "sha50",
						Mergeable:      "MERGEABLE",
						SourcedViaREST: true, RESTMergeableState: "clean", RESTCIFetched: true,
						CIStatus: "SUCCESS", RESTApproved: true,
					},
				}
			}
			return results
		},
	}

	got, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.handleKnownPRConflictsViaREST(context.Background(), got)

	if restMergedNum != 50 {
		t.Fatalf("restMergedNum = %d, want 50 (ready_to_merge must reach handleAutoMerge)", restMergedNum)
	}
	if gqlMergeCalled {
		t.Error("must merge via REST, not the GraphQL gh-pr-merge path")
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
			if got := NewMergeGate(tt.pr).BlockedOnlyByThreads(); got != tt.want {
				t.Errorf("BlockedOnlyByThreads() = %v, want %v", got, tt.want)
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
	r := &Handler{
		logger:   slog.New(slog.DiscardHandler),
		tasks:    tasks,
		projects: projStore,
		agents:   agents,
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
	r.resolveAddressedCopilotThreads(context.Background(), all, prs)

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
	r := &Handler{
		logger:        slog.New(slog.DiscardHandler),
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
	r := &Handler{
		logger:        slog.New(slog.DiscardHandler),
		fetchThreads:  func(string, int) ([]github.ReviewThread, error) { return threads, nil },
		resolveThread: func(id string) error { resolvedIDs = append(resolvedIDs, id); return nil },
	}
	pr := github.PullRequest{Number: 7, Repository: "o/r", Author: "me"}

	r.resolveCopilotThreadsForPR("task1", pr, "")

	if len(resolvedIDs) != 1 || resolvedIDs[0] != "A1" {
		t.Fatalf("resolvedIDs = %v, want [A1] (empty agentLogin must fall back to PR author)", resolvedIDs)
	}
}

// TestHandleTaskPRIssues_ExhaustedRetryParksOnlyWhenNoSiblingHandleable locks
// the priority order in handleTaskPRIssues: escalating to human-required must
// never strand a still-fixable sibling issue on the same push. A ci_failure
// issue that has spent its retry budget (DispatchExhausted) alongside a fresh,
// handleable comments issue on the same PR must dispatch the coalesced fix
// agent, not park the task.
func TestHandleTaskPRIssues_ExhaustedRetryParksOnlyWhenNoSiblingHandleable(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(wfStore.Dir(), "test-pr-fix.yaml"),
		[]byte(mechanicalPRFixYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:   task.Ptr(task.StatusInReview),
		PRNumber: task.Ptr(9001),
	}); err != nil {
		t.Fatal(err)
	}

	prTracker := github.NewIssueTracker(time.Minute)
	// Spend the ci_failure retry budget for this task. ci_failure carries no
	// feedback signature, so Decide caps permanently once retries >= MaxRetries
	// regardless of head SHA (see debounce.go), matching a fix agent that kept
	// failing against the same still-red CI.
	for range github.MaxRetries {
		prTracker.MarkHandled(created.ID, github.PRIssueCIFailure, "sha-exhausted")
	}

	r := &Handler{
		logger:          logger,
		tasks:           tasks,
		agents:          agentMgr,
		prTracker:       prTracker,
		WorkflowEngine:  engine,
		pushPreflightFn: stubPushPreflight(nil),
	}

	pr := github.PullRequest{
		Number: 9001, Repository: "o/r", HeadRefName: "feat", HeadSHA: "sha-exhausted",
		URL: "https://github.com/o/r/pull/9001", FeedbackSig: "sig-fresh",
	}
	issues := []github.PRIssue{
		{Kind: github.PRIssueCIFailure, TaskID: created.ID, PR: pr},
		{Kind: github.PRIssueComments, TaskID: created.ID, PR: pr},
	}

	r.handleTaskPRIssues(context.Background(), created.ID, issues)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want NOT human-required: the handleable comments sibling must dispatch a fix instead of being stranded by ci_failure's exhaustion", got.Status)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched; the handleable comments issue should have triggered a coalesced fix")
	}
	if k := got.Workflow.Variables["pr_issue_kind"]; k != string(github.PRIssueComments) {
		t.Errorf("pr_issue_kind = %q, want %q (only the handleable issue drives dispatch)", k, github.PRIssueComments)
	}
	if got, want := got.Workflow.Variables["pr_issue_kinds"], string(github.PRIssueComments); got != want {
		t.Errorf("pr_issue_kinds = %q, want %q (exhausted ci_failure must not ride along in the dispatched fix)", got, want)
	}
}

// TestHandleKnownPRConflictsViaREST_SkipsCommentsWithoutGraphQLThreadData locks
// the REST-degraded fallback's kind filter: when GraphQL budget is exhausted and
// the monitor falls back to REST-sourced PR fetches (no thread-resolution data),
// a comments issue must never reach dispatch — alone, or riding alongside a
// fixable ci_failure/conflict issue from the same PR — and must never leak into
// the dispatched workflow's kind vars.
func TestHandleKnownPRConflictsViaREST_SkipsCommentsWithoutGraphQLThreadData(t *testing.T) {
	newHarness := func(t *testing.T, prNumber int) (*Handler, *task.Manager, string) {
		t.Helper()
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
		if err := os.WriteFile(filepath.Join(wfStore.Dir(), "test-pr-fix.yaml"),
			[]byte(mechanicalPRFixYAML), 0o644); err != nil {
			t.Fatal(err)
		}
		agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
		engine := workflow.NewEngine(
			wfStore,
			&taskAdapter{tasks: tasks},
			&agentAdapter{agents: agentMgr, tasks: tasks},
			logger,
		)

		created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.Update(created.ID, task.Update{
			Status:    task.Ptr(task.StatusInReview),
			PRNumber:  task.Ptr(prNumber),
			ProjectID: task.Ptr("o/r"),
		}); err != nil {
			t.Fatal(err)
		}
		r := &Handler{
			logger:         logger,
			tasks:          tasks,
			agents:         agentMgr,
			prTracker:      github.NewIssueTracker(time.Minute),
			WorkflowEngine: engine,
		}
		return r, tasks, created.ID
	}

	t.Run("comments dropped alongside a fixable conflict", func(t *testing.T) {
		// Reuses the real project/worktree-backed harness (autoresolve_test.go)
		// since the conflict kind's fix dispatch checks out the PR branch for
		// real (PrepareForFix) — unlike ready_to_merge, it can't be exercised
		// against a bare in-memory Handler.
		h := newAutoResolveHarness(t, false) // auto-resolve off: force through to the agent workflow
		tk, pr := h.newConflictTask(t)
		pr.Mergeable = "CONFLICTING"
		pr.ActionableCount = 1 // draws a comments issue alongside the conflict
		pr.SourcedViaREST = true

		h.r.fetchKnownPRsFn = func(refs []github.PRRef) []github.MonitorPRResult {
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				results[i] = github.MonitorPRResult{Repo: ref.Repo, Number: ref.Number, Open: true, PR: pr}
			}
			return results
		}

		got, err := h.tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		h.r.handleKnownPRConflictsViaREST(context.Background(), got)

		gotTask, err := h.tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gotTask.Workflow == nil {
			t.Fatal("no workflow dispatched; the fixable conflict issue should have routed through")
		}
		if k := gotTask.Workflow.Variables["pr_issue_kind"]; k != string(github.PRIssueConflict) {
			t.Errorf("pr_issue_kind = %q, want %q", k, github.PRIssueConflict)
		}
		if kinds := gotTask.Workflow.Variables["pr_issue_kinds"]; strings.Contains(kinds, string(github.PRIssueComments)) {
			t.Errorf("pr_issue_kinds = %q, must not carry %q (no GraphQL thread data in REST fallback)", kinds, github.PRIssueComments)
		}
		if prompt := gotTask.Workflow.Variables["prompt"]; strings.Contains(prompt, "/fix-review") {
			t.Errorf("dispatched prompt must not address review comments in the REST fallback:\n%s", prompt)
		}
	})

	t.Run("comments-only PR dispatches nothing", func(t *testing.T) {
		const prNumber = 9102
		r, tasks, taskID := newHarness(t, prNumber)
		r.fetchKnownPRsFn = func(refs []github.PRRef) []github.MonitorPRResult {
			results := make([]github.MonitorPRResult, len(refs))
			for i, ref := range refs {
				results[i] = github.MonitorPRResult{
					Repo: ref.Repo, Number: ref.Number, Open: true,
					PR: github.PullRequest{
						Number: ref.Number, Repository: ref.Repo, HeadSHA: "sha-rest-2",
						HeadRefName: "feat", URL: "https://github.com/o/r/pull/9102",
						// PENDING CI keeps this off both ci_failure and ready_to_merge,
						// isolating the comments-only case.
						Mergeable: "MERGEABLE", CIStatus: "PENDING", ActionableCount: 1,
						SourcedViaREST: true,
					},
				}
			}
			return results
		}

		got, err := tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		r.handleKnownPRConflictsViaREST(context.Background(), got)

		gotTask, err := tasks.Get(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if gotTask.Workflow != nil {
			t.Fatalf("workflow dispatched = %+v, want none: a comments-only REST issue must never dispatch", gotTask.Workflow)
		}
		if n := r.prTracker.Retries(taskID, github.PRIssueComments); n != 0 {
			t.Errorf("comments retries = %d, want 0 (never marked handled)", n)
		}
	})
}

// TestEscalateExhaustedFix locks the scope of escalation: every fixable kind
// (conflict, ci_failure, comments) parks a task to human-required once its
// durable retry budget is spent — leaving a capped kind un-escalated would
// strand it (capped, never retried, never surfaced). ready_to_merge never
// escalates, and an already-parked task is left untouched.
func TestEscalateExhaustedFix(t *testing.T) {
	newHandler := func(t *testing.T) (*Handler, *task.Manager, string) {
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
		r := &Handler{
			logger:    slog.New(slog.DiscardHandler),
			tasks:     tasks,
			prTracker: github.NewIssueTracker(30 * time.Minute),
		}
		return r, tasks, created.ID
	}

	t.Run("conflict exhaustion parks to blocked", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueConflict, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusBlocked {
			t.Fatalf("conflict: status = %q, want blocked", got.Status)
		}
		if got.Blocker.Code != string(github.PRIssueConflict) {
			t.Fatalf("conflict: blocker code = %q, want %q", got.Blocker.Code, github.PRIssueConflict)
		}
	})

	t.Run("ci_failure exhaustion parks to blocked", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueCIFailure, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusBlocked {
			t.Fatalf("ci_failure: status = %q, want blocked", got.Status)
		}
	})

	t.Run("fresh escalation clears prior reconciliation latch", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		if _, err := tasks.Update(id, task.Update{
			Tags: task.Ptr([]string{reconciledLatchTag, "keep"}),
		}); err != nil {
			t.Fatalf("pre-set tags: %v", err)
		}
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueConflict, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		for _, tag := range got.Tags {
			if tag == reconciledLatchTag {
				t.Fatalf("reconciliation latch still present after fresh escalation: tags=%v", got.Tags)
			}
		}
		if len(got.Tags) != 1 || got.Tags[0] != "keep" {
			t.Fatalf("tags = %v, want only preserved non-latch tag", got.Tags)
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

	t.Run("comments exhaustion parks to blocked and clears tracker", func(t *testing.T) {
		r, tasks, id := newHandler(t)
		// Spend the budget so the tracker has a non-zero retry count to clear.
		for range github.MaxRetries {
			r.prTracker.MarkHandled(id, github.PRIssueComments, "sha")
		}
		r.escalateExhaustedFix(github.PRIssue{Kind: github.PRIssueComments, TaskID: id, PR: github.PullRequest{Number: 9}})
		got, _ := tasks.Get(id)
		if got.Status != task.StatusBlocked {
			t.Fatalf("comments: status = %q, want blocked", got.Status)
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
