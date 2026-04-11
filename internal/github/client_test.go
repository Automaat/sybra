package github

import (
	"encoding/json"
	"fmt"
	"testing"
)

func resetViewerCache() { resetCachedViewerForTest() }

// fakeExecer is a test double that returns fixed output and error.
type fakeExecer struct {
	output []byte
	err    error
	calls  int
}

func (f *fakeExecer) run(_ ...string) ([]byte, error) {
	f.calls++
	return f.output, f.err
}

// recordingExecer captures the args of the most recent run() invocation
// so tests can assert which command was dispatched.
type recordingExecer struct {
	output   []byte
	err      error
	lastArgs []string
	calls    int
}

func (r *recordingExecer) run(args ...string) ([]byte, error) {
	r.calls++
	r.lastArgs = append([]string(nil), args...)
	return r.output, r.err
}

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

	prs := convertPRs(nodes, "")
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

			prs := convertPRs([]gqlPR{node}, "")
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

			prs := convertPRs(nodes, "")
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

	prs := convertPRs(nodes, "")
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
						StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
					} `json:"commit"`
				}{
					{Commit: struct {
						StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
					}{StatusCheckRollup: &gqlStatusCheckRollup{State: tt.state}}},
				}
			}

			prs := convertPRs([]gqlPR{node}, "")
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

			prs := convertPRs([]gqlPR{node}, "")
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
	prs := convertPRs(nil, "")
	if len(prs) != 0 {
		t.Errorf("got %d PRs for nil input, want 0", len(prs))
	}

	prs = convertPRs([]gqlPR{}, "")
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

	prs := convertPRs(nodes, "")
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

	prs := convertPRs(resp.Data.Search.Nodes, "")
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

	prs := convertPRs(resp.Data.Search.Nodes, "")
	if len(prs) != 0 {
		t.Errorf("got %d PRs, want 0 (bot should be filtered)", len(prs))
	}
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

func TestParsePRURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		url        string
		wantRepo   string
		wantNumber int
	}{
		{
			name:       "valid PR URL",
			url:        "https://github.com/owner/repo/pull/42",
			wantRepo:   "owner/repo",
			wantNumber: 42,
		},
		{
			name:       "valid PR URL large number",
			url:        "https://github.com/org/project/pull/1234",
			wantRepo:   "org/project",
			wantNumber: 1234,
		},
		{
			name: "issue URL not matched",
			url:  "https://github.com/owner/repo/issues/42",
		},
		{
			name: "not github URL",
			url:  "https://gitlab.com/owner/repo/pull/42",
		},
		{
			name: "PR number zero",
			url:  "https://github.com/owner/repo/pull/0",
		},
		{
			name: "non-numeric PR number",
			url:  "https://github.com/owner/repo/pull/abc",
		},
		{
			name: "missing PR number",
			url:  "https://github.com/owner/repo/pull/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, number := ParsePRURL(tt.url)
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if number != tt.wantNumber {
				t.Errorf("number = %d, want %d", number, tt.wantNumber)
			}
		})
	}
}

func TestParseIssueURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		url        string
		wantRepo   string
		wantNumber int
	}{
		{
			name:       "valid issue URL",
			url:        "https://github.com/owner/repo/issues/42",
			wantRepo:   "owner/repo",
			wantNumber: 42,
		},
		{
			name:       "valid issue URL large number",
			url:        "https://github.com/org/project/issues/1234",
			wantRepo:   "org/project",
			wantNumber: 1234,
		},
		{
			name: "PR URL not matched",
			url:  "https://github.com/owner/repo/pull/42",
		},
		{
			name: "not github URL",
			url:  "https://gitlab.com/owner/repo/issues/42",
		},
		{
			name: "issue number zero",
			url:  "https://github.com/owner/repo/issues/0",
		},
		{
			name: "non-numeric issue number",
			url:  "https://github.com/owner/repo/issues/abc",
		},
		{
			name: "missing issue number",
			url:  "https://github.com/owner/repo/issues/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, number := ParseIssueURL(tt.url)
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if number != tt.wantNumber {
				t.Errorf("number = %d, want %d", number, tt.wantNumber)
			}
		})
	}
}
