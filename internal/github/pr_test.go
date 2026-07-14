package github

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFetchPRStateWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		execErr error
		want    PRState
		wantErr bool
	}{
		{
			name:   "merged PR",
			output: `{"state":"MERGED","mergedAt":"2026-04-01T12:00:00Z"}`,
			want:   PRState{State: "MERGED", MergedAt: "2026-04-01T12:00:00Z"},
		},
		{
			name:   "closed PR",
			output: `{"state":"CLOSED","mergedAt":""}`,
			want:   PRState{State: "CLOSED"},
		},
		{
			name:   "open PR",
			output: `{"state":"OPEN","mergedAt":""}`,
			want:   PRState{State: "OPEN"},
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			output:  "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			got, err := fetchPRStateWith(fe, "o/r", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.State != tt.want.State {
				t.Errorf("State = %q, want %q", got.State, tt.want.State)
			}
			if got.MergedAt != tt.want.MergedAt {
				t.Errorf("MergedAt = %q, want %q", got.MergedAt, tt.want.MergedAt)
			}
		})
	}
}

func TestPRState_CIStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		checks []struct{ State string }
		want   string
	}{
		{"no checks", nil, ""},
		{"all success", []struct{ State string }{{"SUCCESS"}, {"NEUTRAL"}}, "SUCCESS"},
		{"has failure", []struct{ State string }{{"SUCCESS"}, {"FAILURE"}}, "FAILURE"},
		{"has error", []struct{ State string }{{"ERROR"}}, "FAILURE"},
		{"has pending", []struct{ State string }{{"SUCCESS"}, {"PENDING"}}, "PENDING"},
		{"failure beats pending", []struct{ State string }{{"PENDING"}, {"FAILURE"}}, "FAILURE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checks := make([]gqlCheckContext, len(tt.checks))
			for i, c := range tt.checks {
				checks[i] = gqlCheckContext{Typename: "StatusContext", State: c.State}
			}
			s := PRState{StatusCheckRollup: checks}
			if got := s.CIStatus(); got != tt.want {
				t.Errorf("CIStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPRState_CIStatus_CheckRunShape verifies the GitHub-Actions/App
// CheckRun shape (`status`/`conclusion`, no `state`) classifies the same as
// the legacy StatusContext shape — this is the exact shape `gh pr view
// --json statusCheckRollup` emits for Actions-based CI, and the shape a
// prior implementation of this task's fix mishandled (see task b569bcef
// test failure: CheckRun entries parsed as permanently SUCCESS because only
// `state` was read).
func TestPRState_CIStatus_CheckRunShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		checks []gqlCheckContext
		want   string
	}{
		{"no checks", nil, ""},
		{
			"all completed success",
			[]gqlCheckContext{{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"}},
			"SUCCESS",
		},
		{
			"in progress is pending, not success",
			[]gqlCheckContext{{Typename: "CheckRun", Status: "IN_PROGRESS", Conclusion: ""}},
			"PENDING",
		},
		{
			"queued is pending",
			[]gqlCheckContext{{Typename: "CheckRun", Status: "QUEUED"}},
			"PENDING",
		},
		{
			"completed failure",
			[]gqlCheckContext{{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"}},
			"FAILURE",
		},
		{
			"failure beats pending across mixed shapes",
			[]gqlCheckContext{
				{Typename: "CheckRun", Status: "IN_PROGRESS"},
				{Typename: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			"FAILURE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := PRState{StatusCheckRollup: tt.checks}
			if got := s.CIStatus(); got != tt.want {
				t.Errorf("CIStatus() = %q, want %q", got, tt.want)
			}
			if tt.want == "PENDING" && !s.HasPendingChecks() {
				t.Errorf("HasPendingChecks() = false, want true for PENDING status")
			}
		})
	}
}

func TestPRState_ReadyToMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		state     PRState
		wantReady bool
	}{
		{"open mergeable no ci", PRState{State: "OPEN", Mergeable: "MERGEABLE"}, true},
		{"open mergeable ci success", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{{Typename: "StatusContext", State: "SUCCESS"}}}, true},
		{"open mergeable success plus pending", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{
			{Typename: "StatusContext", State: "SUCCESS"},
			{Typename: "StatusContext", State: "PENDING"},
		}}, false},
		{"not open", PRState{State: "MERGED", Mergeable: "MERGEABLE"}, false},
		{"conflicting", PRState{State: "OPEN", Mergeable: "CONFLICTING"}, false},
		{"unknown mergeable", PRState{State: "OPEN", Mergeable: "UNKNOWN"}, false},
		{"ci failing", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{{Typename: "StatusContext", State: "FAILURE"}}}, false},
		{"ci pending", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{{Typename: "StatusContext", State: "PENDING"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.ReadyToMerge(); got != tt.wantReady {
				t.Errorf("ReadyToMerge() = %v, want %v", got, tt.wantReady)
			}
		})
	}
}

func TestPRState_Resolved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		state        PRState
		wantResolved bool
	}{
		{"merged", PRState{State: "MERGED"}, true},
		// An abandoned (unmerged) close is NOT treated as resolved: unlike a
		// stale-worktree-vs-green-remote false positive, a human genuinely needs
		// to decide what happens to the task now.
		{"closed", PRState{State: "CLOSED"}, false},
		{"open ready to merge", PRState{State: "OPEN", Mergeable: "MERGEABLE"}, true},
		{"open conflicting", PRState{State: "OPEN", Mergeable: "CONFLICTING"}, false},
		{"open ci pending", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{{Typename: "StatusContext", State: "PENDING"}}}, false},
		{"open ci failing", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []gqlCheckContext{{Typename: "StatusContext", State: "FAILURE"}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.Resolved(); got != tt.wantResolved {
				t.Errorf("Resolved() = %v, want %v", got, tt.wantResolved)
			}
		})
	}
}

func TestFetchPRFilesWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		execErr error
		want    []string
		wantErr bool
	}{
		{
			name:   "multiple files",
			output: `{"files":[{"path":"app.go"},{"path":"internal/task/store.go"},{"path":"main.go"}]}`,
			want:   []string{"app.go", "internal/task/store.go", "main.go"},
		},
		{
			name:   "single file",
			output: `{"files":[{"path":"README.md"}]}`,
			want:   []string{"README.md"},
		},
		{
			name:   "no files",
			output: `{"files":[]}`,
			want:   []string{},
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			output:  "not json",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			got, err := fetchPRFilesWith(fe, "o/r", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d files, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("file[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFetchPRWith_success(t *testing.T) {
	t.Parallel()
	response := `{
		"number": 42,
		"title": "feat: add thing",
		"body": "description",
		"url": "https://github.com/owner/repo/pull/42",
		"headRefName": "feat/add-thing",
		"author": {"login": "dev"},
		"labels": [{"name": "backend"}, {"name": "feature"}]
	}`
	fe := &fakeExecer{output: []byte(response)}
	pr, err := fetchPRWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.Title != "feat: add thing" {
		t.Errorf("Title = %q, want %q", pr.Title, "feat: add thing")
	}
	if pr.HeadRefName != "feat/add-thing" {
		t.Errorf("HeadRefName = %q, want %q", pr.HeadRefName, "feat/add-thing")
	}
	if pr.Author != "dev" {
		t.Errorf("Author = %q, want %q", pr.Author, "dev")
	}
	if pr.Repository != "owner/repo" {
		t.Errorf("Repository = %q, want %q", pr.Repository, "owner/repo")
	}
	if pr.RepoName != "repo" {
		t.Errorf("RepoName = %q, want %q", pr.RepoName, "repo")
	}
	if len(pr.Labels) != 2 || pr.Labels[0] != "backend" || pr.Labels[1] != "feature" {
		t.Errorf("Labels = %v, want [backend feature]", pr.Labels)
	}
}

func TestFetchPRWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{
		output: []byte("not found"),
		err:    fmt.Errorf("exit 1"),
	}
	_, err := fetchPRWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRWith_invalidJSON(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not json")}
	_, err := fetchPRWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFetchPRClosingIssuesWith_sameRepo(t *testing.T) {
	t.Parallel()
	response := `{
		"body": "Initial body",
		"closingIssuesReferences": [
			{"number": 7, "repository": {"name": "repo", "owner": {"login": "owner"}}}
		]
	}`
	fe := &fakeExecer{output: []byte(response)}
	issues, body, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 7 {
		t.Errorf("issues = %v, want [7]", issues)
	}
	if body != "Initial body" {
		t.Errorf("body = %q, want %q", body, "Initial body")
	}
}

func TestFetchPRClosingIssuesWith_filtersCrossRepo(t *testing.T) {
	t.Parallel()
	response := `{
		"body": "",
		"closingIssuesReferences": [
			{"number": 1, "repository": {"name": "repo", "owner": {"login": "owner"}}},
			{"number": 99, "repository": {"name": "other", "owner": {"login": "elsewhere"}}}
		]
	}`
	fe := &fakeExecer{output: []byte(response)}
	issues, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0] != 1 {
		t.Errorf("issues = %v, want [1] (99 belongs to elsewhere/other and must be filtered)", issues)
	}
}

func TestFetchPRClosingIssuesWith_empty(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`{"body": "", "closingIssuesReferences": []}`)}
	issues, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %v, want empty", issues)
	}
}

func TestFetchPRClosingIssuesWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("boom"), err: fmt.Errorf("exit 1")}
	_, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRClosingIssuesWith_invalidJSON(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not json")}
	_, _, err := fetchPRClosingIssuesWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestEditPRBodyWith_passesArgs(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{}
	if err := editPRBodyWith(fe, "owner/repo", 42, "new body with\nnewline"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Args should be: pr edit 42 --repo owner/repo --body <body>
	want := []string{"pr", "edit", "42", "--repo", "owner/repo", "--body", "new body with\nnewline"}
	if len(fe.lastArgs) != len(want) {
		t.Fatalf("args = %v, want %v", fe.lastArgs, want)
	}
	for i, a := range fe.lastArgs {
		if a != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, a, want[i])
		}
	}
}

func TestEditPRBodyWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("forbidden"), err: fmt.Errorf("exit 1")}
	if err := editPRBodyWith(fe, "owner/repo", 42, "body"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRequestReviewersWith_passesArgs(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{output: []byte("HTTP/2.0 201 Created\n\n{}")}
	if err := requestReviewersWith(fe, "owner/repo", 42, []string{"alice", "bob"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"api", "--include", "--method", "POST",
		"repos/owner/repo/pulls/42/requested_reviewers",
		"-f", "reviewers[]=alice",
		"-f", "reviewers[]=bob",
	}
	if len(fe.lastArgs) != len(want) {
		t.Fatalf("args = %v, want %v", fe.lastArgs, want)
	}
	for i, a := range fe.lastArgs {
		if a != want[i] {
			t.Errorf("arg[%d] = %q, want %q", i, a, want[i])
		}
	}
}

func TestRequestReviewersWith_emptySkips(t *testing.T) {
	t.Parallel()
	fe := &recordingExecer{}
	if err := requestReviewersWith(fe, "owner/repo", 42, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fe.calls != 0 {
		t.Fatalf("calls = %d, want 0", fe.calls)
	}
}

func TestRequestReviewersWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("HTTP/2.0 422 Unprocessable Entity\n\nreviewer is not a collaborator"), err: fmt.Errorf("exit 1")}
	if err := requestReviewersWith(fe, "owner/repo", 42, []string{"alice"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFetchPRContextWith_includesAuthorAndCommentAuthors(t *testing.T) {
	t.Parallel()
	fe := &scriptedExecer{results: []scriptedResult{
		{output: []byte(`{
			"url":"https://github.com/owner/repo/pull/42",
			"headRefName":"feature/x",
			"author":{"login":"author"},
			"reviews":[
				{"author":{"login":"alice"},"body":"top level","state":"CHANGES_REQUESTED"},
				{"author":{"login":"ignored"},"body":"approved","state":"APPROVED"}
			]
		}`)},
		{output: []byte("HTTP/2.0 200 OK\n\n" +
			"{\"author\":\"bob\",\"body\":\"inline\",\"path\":\"main.go\"}\n")},
	}}

	got, err := fetchPRContextWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("fetchPRContextWith: %v", err)
	}
	if got.Author != "author" {
		t.Errorf("Author = %q, want author", got.Author)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("comments len = %d, want 2: %+v", len(got.Comments), got.Comments)
	}
	if got.Comments[0].Author != "alice" || got.Comments[1].Author != "bob" {
		t.Fatalf("comment authors = %+v, want alice,bob", got.Comments)
	}
}

func TestMergePRWith(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		execErr error
		output  string
		wantErr bool
	}{
		{
			name:   "success",
			output: "",
		},
		{
			name:    "exec error",
			output:  "gh: not found",
			execErr: fmt.Errorf("exit 1"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fe := &fakeExecer{output: []byte(tt.output), err: tt.execErr}
			err := mergePRWith(fe, "owner/repo", 42)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// scriptedExecer returns a different (output, err) for each call.
type scriptedExecer struct {
	calls    int
	results  []scriptedResult
	lastArgs []string
}

type scriptedResult struct {
	output []byte
	err    error
}

func (s *scriptedExecer) run(args ...string) ([]byte, error) {
	s.lastArgs = args
	i := s.calls
	s.calls++
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	return s.results[i].output, s.results[i].err
}

func TestMergePRWith_RetriesBaseBranchModified(t *testing.T) {
	prev := mergeRetryDelays
	mergeRetryDelays = []time.Duration{0, 0, 0}
	t.Cleanup(func() { mergeRetryDelays = prev })

	t.Run("retries then succeeds", func(t *testing.T) {
		baseModified := []byte("GraphQL: Base branch was modified. Review and try the merge again. (mergePullRequest)")
		se := &scriptedExecer{results: []scriptedResult{
			{output: baseModified, err: fmt.Errorf("exit 1")},
			{output: baseModified, err: fmt.Errorf("exit 1")},
			{output: nil, err: nil},
		}}
		if err := mergePRWith(se, "owner/repo", 42); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if se.calls != 3 {
			t.Fatalf("calls = %d, want 3", se.calls)
		}
	})

	t.Run("gives up after max retries", func(t *testing.T) {
		baseModified := []byte("GraphQL: Base branch was modified. Review and try the merge again.")
		se := &scriptedExecer{results: []scriptedResult{
			{output: baseModified, err: fmt.Errorf("exit 1")},
		}}
		if err := mergePRWith(se, "owner/repo", 42); err == nil {
			t.Fatal("expected error")
		}
		if se.calls != len(mergeRetryDelays)+1 {
			t.Fatalf("calls = %d, want %d", se.calls, len(mergeRetryDelays)+1)
		}
	})

	t.Run("non-retryable error fails fast", func(t *testing.T) {
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("some other error"), err: fmt.Errorf("exit 1")},
		}}
		if err := mergePRWith(se, "owner/repo", 42); err == nil {
			t.Fatal("expected error")
		}
		if se.calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry)", se.calls)
		}
	})
}

func TestEnableAutoMerge(t *testing.T) {
	t.Parallel()
	t.Run("success passes args", func(t *testing.T) {
		t.Parallel()
		fe := &recordingExecer{}
		if err := enableAutoMergeWith(fe, "owner/repo", 42); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"pr", "merge", "42", "--repo", "owner/repo", "--auto", "--squash"}
		if len(fe.lastArgs) != len(want) {
			t.Fatalf("args = %v, want %v", fe.lastArgs, want)
		}
		for i, a := range fe.lastArgs {
			if a != want[i] {
				t.Errorf("arg[%d] = %q, want %q", i, a, want[i])
			}
		}
	})

	t.Run("gh error passthrough", func(t *testing.T) {
		t.Parallel()
		fe := &fakeExecer{output: []byte("gh: pull request is in unmergeable state"), err: fmt.Errorf("exit 1")}
		err := enableAutoMergeWith(fe, "owner/repo", 42)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "gh pr merge --auto 42") {
			t.Errorf("error = %v, want it to mention 'gh pr merge --auto 42'", err)
		}
	})

	// enableAutoMergeWith invalidates the PR caches on success exactly like
	// mergePRWith / markReadyWith (`if runtimeCacheEnabled(e) {
	// invalidatePRCaches(...) }`), gated off for any non-default execer — the
	// same idiom every other mutating call in this file already follows and
	// already exercises via runtimeCacheEnabled's e == defaultExecer check.
	t.Run("non-default execer never touches the cache", func(t *testing.T) {
		t.Parallel()
		key := prCacheKey("owner/repo", 99)
		prStateCache.Set(key, PRState{State: "OPEN"}, time.Minute)
		fe := &recordingExecer{}
		if err := enableAutoMergeWith(fe, "owner/repo", 99); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := prStateCache.Get(key); !ok {
			t.Error("cache entry was invalidated by a non-default execer, want untouched")
		}
	})
}

func TestSupportsNativeAutoMerge(t *testing.T) {
	t.Parallel()
	repoAllowed := `{"allow_auto_merge":true}`
	repoDisallowed := `{"allow_auto_merge":false}`
	protectionFull := `{"required_status_checks":{"contexts":["ci/build"]},"required_conversation_resolution":{"enabled":true}}`
	protectionNoChecks := `{"required_status_checks":{"contexts":[]},"required_conversation_resolution":{"enabled":true}}`
	protectionNoConvoResolution := `{"required_status_checks":{"contexts":["ci/build"]},"required_conversation_resolution":{"enabled":false}}`

	tests := []struct {
		name    string
		results []scriptedResult
		want    bool
		wantErr bool
	}{
		{
			name: "allow_auto_merge false -> unsupported",
			results: []scriptedResult{
				{output: []byte(repoDisallowed)},
			},
			want: false,
		},
		{
			name: "no required status checks -> unsupported",
			results: []scriptedResult{
				{output: []byte(repoAllowed)},
				{output: []byte(protectionNoChecks)},
			},
			want: false,
		},
		{
			name: "conversation resolution not required -> unsupported",
			results: []scriptedResult{
				{output: []byte(repoAllowed)},
				{output: []byte(protectionNoConvoResolution)},
			},
			want: false,
		},
		{
			name: "branch protection lookup 404/error -> unsupported, not an error",
			results: []scriptedResult{
				{output: []byte(repoAllowed)},
				{output: []byte("gh: HTTP 404: Not Found"), err: fmt.Errorf("exit 1")},
			},
			want: false,
		},
		{
			name: "repo settings lookup error -> unsupported, not an error",
			results: []scriptedResult{
				{output: []byte("gh: HTTP 404: Not Found"), err: fmt.Errorf("exit 1")},
			},
			want: false,
		},
		{
			name: "all conditions met -> supported",
			results: []scriptedResult{
				{output: []byte(repoAllowed)},
				{output: []byte(protectionFull)},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			se := &scriptedExecer{results: tt.results}
			got, err := supportsNativeAutoMergeWith(se, "owner/repo", "main")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("supportsNativeAutoMergeWith() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("malformed JSON on 200 response is an error", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("not json")},
		}}
		if _, err := supportsNativeAutoMergeWith(se, "owner/repo", "main"); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("called with base branch, not head branch", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte(repoAllowed)},
			{output: []byte(protectionFull)},
		}}
		if _, err := supportsNativeAutoMergeWith(se, "owner/repo", "release/base"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(se.lastArgs[len(se.lastArgs)-1], "release%2Fbase") {
			t.Errorf("last call args = %v, want it to reference path-escaped base branch release%%2Fbase", se.lastArgs)
		}
		if strings.Contains(se.lastArgs[len(se.lastArgs)-1], "branches/release/base/") {
			t.Errorf("last call args = %v, base branch slash must be escaped, not a raw path separator", se.lastArgs)
		}
		for _, a := range se.lastArgs {
			if strings.Contains(a, "feature/head") {
				t.Errorf("call args = %v, must never reference a head branch", se.lastArgs)
			}
		}
	})
}

func TestMergePRViaRESTWith(t *testing.T) {
	t.Parallel()

	t.Run("success carries head sha and PUT method", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("HTTP/1.1 200 OK\n\n{\"merged\":true}"), err: nil},
		}}
		if err := mergePRViaRESTWith(se, "owner/repo", 42, "abc123"); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		joined := strings.Join(se.lastArgs, " ")
		if !strings.Contains(joined, "repos/owner/repo/pulls/42/merge") {
			t.Errorf("args missing merge endpoint: %s", joined)
		}
		if !strings.Contains(joined, "PUT") {
			t.Errorf("args missing PUT method: %s", joined)
		}
		if !strings.Contains(joined, "sha=abc123") {
			t.Errorf("args missing head sha: %s", joined)
		}
	})

	t.Run("empty head sha is rejected before any request", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("HTTP/1.1 200 OK\n\n{\"merged\":true}"), err: nil},
		}}
		if err := mergePRViaRESTWith(se, "owner/repo", 42, ""); err == nil {
			t.Fatal("expected error for empty head sha")
		}
		if se.calls != 0 {
			t.Fatalf("calls = %d, want 0 (no request without a head sha)", se.calls)
		}
	})

	t.Run("head sha mismatch (409) is terminal, no retry", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("HTTP/1.1 409 Conflict\n\n{\"message\":\"Head branch was modified. Review and try the merge again.\"}"), err: fmt.Errorf("exit 1")},
		}}
		if err := mergePRViaRESTWith(se, "owner/repo", 42, "abc123"); err == nil {
			t.Fatal("expected error")
		}
		if se.calls != 1 {
			t.Fatalf("calls = %d, want 1 (no retry on sha mismatch)", se.calls)
		}
	})

	t.Run("generic 409 without the mismatch message is not treated as terminal", func(t *testing.T) {
		t.Parallel()
		se := &scriptedExecer{results: []scriptedResult{
			{output: []byte("HTTP/1.1 409 Conflict\n\n{\"message\":\"some other conflict\"}"), err: fmt.Errorf("exit 1")},
		}}
		if err := mergePRViaRESTWith(se, "owner/repo", 42, "abc123"); err == nil {
			t.Fatal("expected error")
		}
		if isHeadSHAMismatchErr(ghHTTPResponse{statusCode: 409, body: []byte("some other conflict")}) {
			t.Fatal("generic 409 must not be classified as a head-SHA mismatch")
		}
	})

	t.Run("base branch modified retries then succeeds", func(t *testing.T) {
		prev := mergeRetryDelays
		mergeRetryDelays = []time.Duration{0, 0, 0}
		t.Cleanup(func() { mergeRetryDelays = prev })

		baseModified := []byte("HTTP/1.1 405 Method Not Allowed\n\nBase branch was modified. Review and try the merge again.")
		se := &scriptedExecer{results: []scriptedResult{
			{output: baseModified, err: fmt.Errorf("exit 1")},
			{output: baseModified, err: fmt.Errorf("exit 1")},
			{output: []byte("HTTP/1.1 200 OK\n\n{\"merged\":true}"), err: nil},
		}}
		if err := mergePRViaRESTWith(se, "owner/repo", 42, "abc123"); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if se.calls != 3 {
			t.Fatalf("calls = %d, want 3", se.calls)
		}
	})
}
