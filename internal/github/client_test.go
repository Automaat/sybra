package github

import (
	"encoding/json"
	"fmt"
	"testing"
)

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

type sequenceExecer struct {
	outputs [][]byte
	errs    []error
	calls   int
}

func (s *sequenceExecer) run(_ ...string) ([]byte, error) {
	i := s.calls
	s.calls++
	if i >= len(s.outputs) {
		return nil, fmt.Errorf("unexpected call %d", i+1)
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.outputs[i], err
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
						OID               string                `json:"oid"`
						StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
					} `json:"commit"`
				}{
					{Commit: struct {
						OID               string                `json:"oid"`
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
				var n threadNode
				n.IsResolved = resolved
				// Each unresolved thread's last comment is a reviewer's, so it is
				// also actionable (the ball is in the agent's court).
				n.Comments.Nodes = []commentNode{{Author: authorLogin("reviewer")}}
				node.ReviewThreads.Nodes = append(node.ReviewThreads.Nodes, n)
			}

			prs := convertPRs([]gqlPR{node}, "user")
			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			if prs[0].UnresolvedCount != tt.expected {
				t.Errorf("UnresolvedCount = %d, want %d", prs[0].UnresolvedCount, tt.expected)
			}
			if prs[0].ActionableCount != tt.expected {
				t.Errorf("ActionableCount = %d, want %d", prs[0].ActionableCount, tt.expected)
			}
		})
	}
}

// threadNode / commentNode mirror the anonymous gqlPR.ReviewThreads node shape
// so tests can build review-thread fixtures without spelling out the nested
// anonymous structs at every call site.
type threadNode = struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	Comments   struct {
		Nodes []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

type commentNode = struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
}

func authorLogin(login string) struct {
	Login string `json:"login"`
} {
	return struct {
		Login string `json:"login"`
	}{Login: login}
}

// TestConvertPRs_actionableAndSig locks two properties: (1) a thread the agent
// replied to is unresolved but NOT actionable (so pr-fix stops firing), and
// (2) the feedback signature is stable across the agent's own replies but
// changes when the reviewer opens a new thread (so the retry budget caps on
// stale feedback yet resets on genuinely new feedback).
func TestConvertPRs_actionableAndSig(t *testing.T) {
	t.Parallel()
	// mk builds a PR whose unresolved threads each have the given last-comment
	// author. Thread IDs are positional ("T0", "T1", ...).
	mk := func(lastAuthors ...string) PullRequest {
		var n gqlPR
		n.Number = 1
		n.Author.Login = "me"
		n.Author.Type = "User"
		n.Repository.NameWithOwner = "o/r"
		for i, la := range lastAuthors {
			var th threadNode
			th.ID = "T" + string(rune('0'+i))
			th.Comments.Nodes = []commentNode{{Author: authorLogin(la)}}
			n.ReviewThreads.Nodes = append(n.ReviewThreads.Nodes, th)
		}
		return convertPRs([]gqlPR{n}, "me")[0]
	}

	const copilot = "copilot-pull-request-reviewer[bot]"

	// Reviewer had the last word → actionable, non-empty signature.
	rev := mk(copilot)
	if rev.ActionableCount != 1 || rev.UnresolvedCount != 1 || rev.FeedbackSig == "" {
		t.Fatalf("reviewer-last: actionable=%d unresolved=%d sig=%q, want 1/1/non-empty",
			rev.ActionableCount, rev.UnresolvedCount, rev.FeedbackSig)
	}

	// Agent (viewer "me") replied last → unresolved but NOT actionable, and the
	// signature is unchanged (same thread set) — the agent's reply must not reset
	// the retry budget.
	addr := mk("me")
	if addr.ActionableCount != 0 || addr.UnresolvedCount != 1 {
		t.Fatalf("agent-last: actionable=%d unresolved=%d, want 0/1", addr.ActionableCount, addr.UnresolvedCount)
	}
	if addr.FeedbackSig != rev.FeedbackSig {
		t.Error("signature changed when the agent replied (same thread set) — would reset the budget")
	}

	// A new reviewer thread changes the thread set → new signature → fresh budget.
	rev2 := mk(copilot, copilot)
	if rev2.FeedbackSig == rev.FeedbackSig {
		t.Error("new reviewer thread did not change the signature")
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

func TestIsCopilotReviewer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		login string
		want  bool
	}{
		{"Copilot", true},
		{"copilot-pull-request-reviewer", true},
		{"copilot-pull-request-reviewer[bot]", true},
		{"github-copilot[bot]", true},
		{"dev", false},
		{"renovate[bot]", false},
		{"copilotuser", false},                        // human login containing the word
		{"copilot-metrics[bot]", false},               // unrelated 3rd-party app
		{"copilot-pull-request-reviewer-evil", false}, // prefix must not pass
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.login, func(t *testing.T) {
			t.Parallel()
			if got := IsCopilotReviewer(tt.login); got != tt.want {
				t.Errorf("IsCopilotReviewer(%q) = %v, want %v", tt.login, got, tt.want)
			}
		})
	}
}

func TestParseGQLResponse_copilotReviewed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reviews string
		want    bool
	}{
		{"copilot reviewed", `{"state": "COMMENTED", "author": {"login": "copilot-pull-request-reviewer[bot]"}}`, true},
		{"only human reviewed", `{"state": "APPROVED", "author": {"login": "dev"}}`, false},
		{"no reviews", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := `{"data":{"search":{"nodes":[{
				"number": 7,
				"repository": {"name": "r", "nameWithOwner": "o/r"},
				"author": {"login": "dev", "type": "User"},
				"latestReviews": {"nodes": [` + tt.reviews + `]}
			}]}}}`
			var resp gqlResponse
			if err := json.Unmarshal([]byte(raw), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			prs := convertPRs(resp.Data.Search.Nodes, "")
			if len(prs) != 1 {
				t.Fatalf("got %d PRs, want 1", len(prs))
			}
			if prs[0].CopilotReviewed != tt.want {
				t.Errorf("CopilotReviewed = %v, want %v", prs[0].CopilotReviewed, tt.want)
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

func TestIsTransientError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		errMsg    string
		transient bool
	}{
		{"nil error", "", false},
		{"504 html page", "gh api graphql: gh: HTTP 504: exit status 1", true},
		{"502 html page", "gh api graphql: gh: HTTP 502: exit status 1", true},
		{"500 internal", "gh api graphql: gh: HTTP 500: exit status 1", true},
		{"dial tcp timeout", "gh api graphql: dial tcp 1.2.3.4:443: i/o timeout: exit status 1", true},
		{"i/o timeout bare", "i/o timeout", true},
		{"context deadline exceeded", "context deadline exceeded", true},
		{"401 auth", "gh api graphql: gh: HTTP 401: exit status 1", false},
		{"404 not found", "gh api graphql: gh: HTTP 404: exit status 1", false},
		{"graphql error", "graphql: Field 'foo' doesn't exist", false},
		{"parse error", "parse graphql response: unexpected EOF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tt.errMsg != "" {
				err = fmt.Errorf("%s", tt.errMsg)
			}
			if got := IsTransientError(err); got != tt.transient {
				t.Errorf("IsTransientError(%q) = %v, want %v", tt.errMsg, got, tt.transient)
			}
		})
	}
}

func TestSanitizeGHOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text passthrough",
			in:   "  gh: not logged in  ",
			want: "gh: not logged in",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "html unicorn 504 collapses to status line",
			in:   "<!DOCTYPE html>\n<html><body>Unicorn</body></html>\ngh: HTTP 504",
			want: "gh: HTTP 504",
		},
		{
			name: "html with no trailing gh line",
			in:   "<!DOCTYPE html><html><body>Unicorn</body></html>",
			want: "GitHub returned an HTML error page",
		},
		{
			name: "lowercase html tag",
			in:   "<html><body>x</body></html>\ngh: HTTP 502",
			want: "gh: HTTP 502",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeGHOutput([]byte(tt.in)); got != tt.want {
				t.Errorf("sanitizeGHOutput = %q, want %q", got, tt.want)
			}
		})
	}
}
