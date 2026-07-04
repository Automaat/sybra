package review

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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
			if got := readyForCopilotAutoMerge(tt.pr); got != tt.want {
				t.Errorf("readyForCopilotAutoMerge() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadyForRESTAutoMerge(t *testing.T) {
	t.Parallel()
	base := github.PullRequest{
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
			if got := readyForRESTAutoMerge(tt.pr); got != tt.want {
				t.Errorf("readyForRESTAutoMerge() = %v, want %v", got, tt.want)
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
			if got := readyToArmNativeAutoMerge(tt.pr); got != tt.want {
				t.Errorf("readyToArmNativeAutoMerge() = %v, want %v", got, tt.want)
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

			r.handleAutoMerge(github.PRIssue{
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

			r.handleAutoMerge(github.PRIssue{
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

	r.handleAutoMerge(github.PRIssue{
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
