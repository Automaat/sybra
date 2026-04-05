package github

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestConvertPRs_basic(t *testing.T) {
	t.Parallel()
	nodes := []gqlPR{
		{
			Number:         42,
			Title:          "feat: add thing",
			URL:            "https://github.com/org/repo/pull/42",
			IsDraft:        false,
			Mergeable:      "MERGEABLE",
			CreatedAt:      "2026-04-01T00:00:00Z",
			UpdatedAt:      "2026-04-02T00:00:00Z",
			ReviewDecision: "APPROVED",
		},
	}
	nodes[0].Author.Login = "user1"
	nodes[0].Author.Type = "User"
	nodes[0].Repository.Name = "repo"
	nodes[0].Repository.NameWithOwner = "org/repo"

	prs := convertPRs(nodes)
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}

	pr := prs[0]
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.Title != "feat: add thing" {
		t.Errorf("Title = %q, want %q", pr.Title, "feat: add thing")
	}
	if pr.Repository != "org/repo" {
		t.Errorf("Repository = %q, want %q", pr.Repository, "org/repo")
	}
	if pr.RepoName != "repo" {
		t.Errorf("RepoName = %q, want %q", pr.RepoName, "repo")
	}
	if pr.Author != "user1" {
		t.Errorf("Author = %q, want %q", pr.Author, "user1")
	}
	if pr.ReviewDecision != "APPROVED" {
		t.Errorf("ReviewDecision = %q, want %q", pr.ReviewDecision, "APPROVED")
	}
	if pr.Mergeable != "MERGEABLE" {
		t.Errorf("Mergeable = %q, want %q", pr.Mergeable, "MERGEABLE")
	}
}

func TestConvertPRs_mergeable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		mergeable string
		want      string
	}{
		{"mergeable", "MERGEABLE", "MERGEABLE"},
		{"conflicting", "CONFLICTING", "CONFLICTING"},
		{"unknown", "UNKNOWN", "UNKNOWN"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			node := gqlPR{
				Number:    1,
				Title:     "test",
				URL:       "https://example.com",
				Mergeable: tt.mergeable,
			}
			node.Author.Login = "user"
			node.Author.Type = "User"
			node.Repository.Name = "repo"
			node.Repository.NameWithOwner = "org/repo"

			prs := convertPRs([]gqlPR{node})
			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			if prs[0].Mergeable != tt.want {
				t.Errorf("Mergeable = %q, want %q", prs[0].Mergeable, tt.want)
			}
		})
	}
}

func TestConvertPRs_filtersBot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		login     string
		typeName  string
		wantCount int
	}{
		{"Bot type", "renovate", "Bot", 0},
		{"bot suffix", "dependabot[bot]", "User", 0},
		{"normal user", "developer", "User", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			nodes := []gqlPR{{
				Number: 1,
				Title:  "test",
				URL:    "https://example.com",
			}}
			nodes[0].Author.Login = tt.login
			nodes[0].Author.Type = tt.typeName
			nodes[0].Repository.Name = "repo"
			nodes[0].Repository.NameWithOwner = "org/repo"

			prs := convertPRs(nodes)
			if len(prs) != tt.wantCount {
				t.Errorf("got %d PRs, want %d for %s/%s", len(prs), tt.wantCount, tt.typeName, tt.login)
			}
		})
	}
}

func TestConvertPRs_labels(t *testing.T) {
	t.Parallel()
	nodes := []gqlPR{{
		Number: 1,
		Title:  "test",
		URL:    "https://example.com",
	}}
	nodes[0].Author.Login = "user"
	nodes[0].Author.Type = "User"
	nodes[0].Repository.Name = "repo"
	nodes[0].Repository.NameWithOwner = "org/repo"
	nodes[0].Labels.Nodes = []struct {
		Name string `json:"name"`
	}{
		{Name: "bug"},
		{Name: "priority"},
	}

	prs := convertPRs(nodes)
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	if len(prs[0].Labels) != 2 {
		t.Fatalf("got %d labels, want 2", len(prs[0].Labels))
	}
	if prs[0].Labels[0] != "bug" {
		t.Errorf("Labels[0] = %q, want %q", prs[0].Labels[0], "bug")
	}
	if prs[0].Labels[1] != "priority" {
		t.Errorf("Labels[1] = %q, want %q", prs[0].Labels[1], "priority")
	}
}

func TestConvertPRs_ciStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		state  string
		hasCI  bool
		expect string
	}{
		{"success", "SUCCESS", true, "SUCCESS"},
		{"failure", "FAILURE", true, "FAILURE"},
		{"pending", "PENDING", true, "PENDING"},
		{"no checks", "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			node := gqlPR{
				Number: 1,
				Title:  "test",
				URL:    "https://example.com",
			}
			node.Author.Login = "user"
			node.Author.Type = "User"
			node.Repository.Name = "repo"
			node.Repository.NameWithOwner = "org/repo"

			if tt.hasCI {
				node.Commits.Nodes = []struct {
					Commit struct {
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					} `json:"commit"`
				}{
					{Commit: struct {
						StatusCheckRollup *struct {
							State string `json:"state"`
						} `json:"statusCheckRollup"`
					}{StatusCheckRollup: &struct {
						State string `json:"state"`
					}{State: tt.state}}},
				}
			}

			prs := convertPRs([]gqlPR{node})
			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			if prs[0].CIStatus != tt.expect {
				t.Errorf("CIStatus = %q, want %q", prs[0].CIStatus, tt.expect)
			}
		})
	}
}

func TestConvertPRs_unresolvedThreads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		threads  []bool
		expected int
	}{
		{"all resolved", []bool{true, true, true}, 0},
		{"one unresolved", []bool{true, false, true}, 1},
		{"all unresolved", []bool{false, false}, 2},
		{"no threads", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			node := gqlPR{
				Number: 1,
				Title:  "test",
				URL:    "https://example.com",
			}
			node.Author.Login = "user"
			node.Author.Type = "User"
			node.Repository.Name = "repo"
			node.Repository.NameWithOwner = "org/repo"

			for _, resolved := range tt.threads {
				node.ReviewThreads.Nodes = append(node.ReviewThreads.Nodes, struct {
					IsResolved bool `json:"isResolved"`
				}{IsResolved: resolved})
			}

			prs := convertPRs([]gqlPR{node})
			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			if prs[0].UnresolvedCount != tt.expected {
				t.Errorf("UnresolvedCount = %d, want %d", prs[0].UnresolvedCount, tt.expected)
			}
		})
	}
}

func TestConvertPRs_emptyInput(t *testing.T) {
	t.Parallel()
	prs := convertPRs(nil)
	if len(prs) != 0 {
		t.Errorf("got %d PRs for nil input, want 0", len(prs))
	}

	prs = convertPRs([]gqlPR{})
	if len(prs) != 0 {
		t.Errorf("got %d PRs for empty input, want 0", len(prs))
	}
}

func TestConvertPRs_mixedBotAndUser(t *testing.T) {
	t.Parallel()
	nodes := []gqlPR{
		{Number: 1, Title: "bot pr", URL: "https://example.com/1"},
		{Number: 2, Title: "user pr", URL: "https://example.com/2"},
		{Number: 3, Title: "another bot", URL: "https://example.com/3"},
	}
	nodes[0].Author.Login = "renovate"
	nodes[0].Author.Type = "Bot"
	nodes[0].Repository.Name = "r"
	nodes[0].Repository.NameWithOwner = "o/r"

	nodes[1].Author.Login = "dev"
	nodes[1].Author.Type = "User"
	nodes[1].Repository.Name = "r"
	nodes[1].Repository.NameWithOwner = "o/r"

	nodes[2].Author.Login = "dependabot[bot]"
	nodes[2].Author.Type = "User"
	nodes[2].Repository.Name = "r"
	nodes[2].Repository.NameWithOwner = "o/r"

	prs := convertPRs(nodes)
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	if prs[0].Title != "user pr" {
		t.Errorf("Title = %q, want %q", prs[0].Title, "user pr")
	}
}

func TestIsBot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typeName string
		login    string
		want     bool
	}{
		{"Bot", "renovate", true},
		{"User", "dependabot[bot]", true},
		{"Bot", "some-app[bot]", true},
		{"User", "developer", false},
		{"Organization", "org", false},
	}

	for _, tt := range tests {
		t.Run(tt.login, func(t *testing.T) {
			t.Parallel()
			if got := isBot(tt.typeName, tt.login); got != tt.want {
				t.Errorf("isBot(%q, %q) = %v, want %v", tt.typeName, tt.login, got, tt.want)
			}
		})
	}
}

func TestParseGQLResponse(t *testing.T) {
	t.Parallel()
	raw := `{
		"data": {
			"search": {
				"nodes": [
					{
						"number": 10,
						"title": "test PR",
						"url": "https://github.com/o/r/pull/10",
						"isDraft": true,
						"createdAt": "2026-01-01T00:00:00Z",
						"updatedAt": "2026-01-02T00:00:00Z",
						"reviewDecision": "CHANGES_REQUESTED",
						"author": {"login": "dev", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": [{"name": "urgent"}]},
						"commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": "FAILURE"}}}]},
						"reviewThreads": {"nodes": [{"isResolved": false}, {"isResolved": true}]}
					}
				]
			}
		}
	}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	prs := convertPRs(resp.Data.Search.Nodes)
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}

	pr := prs[0]
	if pr.Number != 10 {
		t.Errorf("Number = %d, want 10", pr.Number)
	}
	if !pr.IsDraft {
		t.Error("IsDraft = false, want true")
	}
	if pr.CIStatus != "FAILURE" {
		t.Errorf("CIStatus = %q, want FAILURE", pr.CIStatus)
	}
	if pr.ReviewDecision != "CHANGES_REQUESTED" {
		t.Errorf("ReviewDecision = %q, want CHANGES_REQUESTED", pr.ReviewDecision)
	}
	if pr.UnresolvedCount != 1 {
		t.Errorf("UnresolvedCount = %d, want 1", pr.UnresolvedCount)
	}
	if len(pr.Labels) != 1 || pr.Labels[0] != "urgent" {
		t.Errorf("Labels = %v, want [urgent]", pr.Labels)
	}
}

func TestParseGQLResponse_errors(t *testing.T) {
	t.Parallel()
	raw := `{"data":{"search":{"nodes":[]}},"errors":[{"message":"rate limited"}]}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(resp.Errors))
	}
	if resp.Errors[0].Message != "rate limited" {
		t.Errorf("error message = %q, want %q", resp.Errors[0].Message, "rate limited")
	}
}

type fakeExecer struct {
	output []byte
	err    error
	calls  int
}

func (f *fakeExecer) run(_ ...string) ([]byte, error) {
	f.calls++
	return f.output, f.err
}

func TestSearchPRsWith_success(t *testing.T) {
	t.Parallel()
	response := `{
		"data": {
			"search": {
				"nodes": [
					{
						"number": 5,
						"title": "test",
						"url": "https://github.com/o/r/pull/5",
						"author": {"login": "dev", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []}
					}
				]
			}
		}
	}`

	fe := &fakeExecer{output: []byte(response)}
	prs, err := searchPRsWith(fe, "is:pr is:open")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	if prs[0].Number != 5 {
		t.Errorf("Number = %d, want 5", prs[0].Number)
	}
}

func TestSearchPRsWith_execError(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{
		output: []byte("gh: not logged in"),
		err:    fmt.Errorf("exit status 1"),
	}
	_, err := searchPRsWith(fe, "is:pr")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" {
		t.Error("error should contain message")
	}
}

func TestSearchPRsWith_invalidJSON(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not json")}
	_, err := searchPRsWith(fe, "is:pr")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSearchPRsWith_graphqlError(t *testing.T) {
	t.Parallel()
	response := `{"data":{"search":{"nodes":[]}},"errors":[{"message":"rate limited"}]}`
	fe := &fakeExecer{output: []byte(response)}
	_, err := searchPRsWith(fe, "is:pr")
	if err == nil {
		t.Fatal("expected error for graphql error")
	}
	if got := err.Error(); got != "graphql: rate limited" {
		t.Errorf("error = %q, want %q", got, "graphql: rate limited")
	}
}

func TestFetchReviewsWith_success(t *testing.T) {
	t.Parallel()
	response := `{
		"data": {
			"search": {
				"nodes": [
					{
						"number": 1,
						"title": "my PR",
						"url": "https://github.com/o/r/pull/1",
						"author": {"login": "me", "type": "User"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []}
					}
				]
			}
		}
	}`

	fe := &fakeExecer{output: []byte(response)}
	summary, err := fetchReviewsWith(fe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fe.calls != 2 {
		t.Errorf("expected 2 calls (created + requested), got %d", fe.calls)
	}
	if len(summary.CreatedByMe) != 1 {
		t.Errorf("CreatedByMe len = %d, want 1", len(summary.CreatedByMe))
	}
	if len(summary.ReviewRequested) != 1 {
		t.Errorf("ReviewRequested len = %d, want 1", len(summary.ReviewRequested))
	}
}

func TestFetchReviewsWith_firstCallFails(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{
		output: []byte("auth error"),
		err:    fmt.Errorf("exit 1"),
	}
	_, err := fetchReviewsWith(fe)
	if err == nil {
		t.Fatal("expected error")
	}
}

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
			checks := make([]struct {
				State string `json:"state"`
			}, len(tt.checks))
			for i, c := range tt.checks {
				checks[i].State = c.State
			}
			s := PRState{StatusCheckRollup: checks}
			if got := s.CIStatus(); got != tt.want {
				t.Errorf("CIStatus() = %q, want %q", got, tt.want)
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
		{"open mergeable ci success", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"SUCCESS"}}}, true},
		{"not open", PRState{State: "MERGED", Mergeable: "MERGEABLE"}, false},
		{"conflicting", PRState{State: "OPEN", Mergeable: "CONFLICTING"}, false},
		{"unknown mergeable", PRState{State: "OPEN", Mergeable: "UNKNOWN"}, false},
		{"ci failing", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"FAILURE"}}}, false},
		{"ci pending", PRState{State: "OPEN", Mergeable: "MERGEABLE", StatusCheckRollup: []struct {
			State string `json:"state"`
		}{{"PENDING"}}}, false},
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

func TestParseGQLResponse_botFiltered(t *testing.T) {
	t.Parallel()
	raw := `{
		"data": {
			"search": {
				"nodes": [
					{
						"number": 1,
						"title": "bot PR",
						"url": "https://example.com",
						"author": {"login": "renovate", "type": "Bot"},
						"repository": {"name": "r", "nameWithOwner": "o/r"},
						"labels": {"nodes": []},
						"commits": {"nodes": []},
						"reviewThreads": {"nodes": []}
					}
				]
			}
		}
	}`

	var resp gqlResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	prs := convertPRs(resp.Data.Search.Nodes)
	if len(prs) != 0 {
		t.Errorf("got %d PRs, want 0 (bot should be filtered)", len(prs))
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

func TestHasPendingReview_pending(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[{"state":"COMMENTED"},{"state":"PENDING"}]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected pending review, got false")
	}
}

func TestHasPendingReview_noPending(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[{"state":"APPROVED"},{"state":"COMMENTED"}]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected no pending review, got true")
	}
}

func TestHasPendingReview_empty(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte(`[]`)}
	got, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected no pending review, got true")
	}
}

func TestHasPendingReview_error(t *testing.T) {
	t.Parallel()
	fe := &fakeExecer{output: []byte("not found"), err: fmt.Errorf("exit 1")}
	_, err := hasPendingReviewWith(fe, "owner/repo", 42)
	if err == nil {
		t.Fatal("expected error")
	}
}
