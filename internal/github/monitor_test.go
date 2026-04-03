package github

import "testing"

func TestMatchTaskPRs(t *testing.T) {
	tests := []struct {
		name  string
		prs   []PullRequest
		tasks []TaskMatcher
		want  []PRIssue
	}{
		{
			name:  "no PRs",
			prs:   nil,
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  nil,
		},
		{
			name:  "no matching task",
			prs:   []PullRequest{{Number: 99, CIStatus: "FAILURE"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  nil,
		},
		{
			name:  "match by PR number, CI failure",
			prs:   []PullRequest{{Number: 42, CIStatus: "FAILURE", Repository: "o/r"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  []PRIssue{{Kind: PRIssueCIFailure, TaskID: "t1", PR: PullRequest{Number: 42, CIStatus: "FAILURE", Repository: "o/r"}}},
		},
		{
			name:  "match by branch, conflict",
			prs:   []PullRequest{{Number: 10, HeadRefName: "synapse/fix-abc", Mergeable: "CONFLICTING"}},
			tasks: []TaskMatcher{{ID: "t2", Branch: "synapse/fix-abc"}},
			want:  []PRIssue{{Kind: PRIssueConflict, TaskID: "t2", PR: PullRequest{Number: 10, HeadRefName: "synapse/fix-abc", Mergeable: "CONFLICTING"}}},
		},
		{
			name:  "both conflict and CI failure",
			prs:   []PullRequest{{Number: 42, CIStatus: "FAILURE", Mergeable: "CONFLICTING"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want: []PRIssue{
				{Kind: PRIssueConflict, TaskID: "t1", PR: PullRequest{Number: 42, CIStatus: "FAILURE", Mergeable: "CONFLICTING"}},
				{Kind: PRIssueCIFailure, TaskID: "t1", PR: PullRequest{Number: 42, CIStatus: "FAILURE", Mergeable: "CONFLICTING"}},
			},
		},
		{
			name:  "draft PR still monitored",
			prs:   []PullRequest{{Number: 42, IsDraft: true, CIStatus: "FAILURE"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  []PRIssue{{Kind: PRIssueCIFailure, TaskID: "t1", PR: PullRequest{Number: 42, IsDraft: true, CIStatus: "FAILURE"}}},
		},
		{
			name:  "UNKNOWN mergeable is not conflict",
			prs:   []PullRequest{{Number: 42, Mergeable: "UNKNOWN"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  nil,
		},
		{
			name:  "PENDING CI is not failure",
			prs:   []PullRequest{{Number: 42, CIStatus: "PENDING"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  nil,
		},
		{
			name:  "SUCCESS CI no issue",
			prs:   []PullRequest{{Number: 42, CIStatus: "SUCCESS", Mergeable: "MERGEABLE"}},
			tasks: []TaskMatcher{{ID: "t1", PRNumber: 42}},
			want:  nil,
		},
		{
			name: "PR number takes precedence over branch",
			prs:  []PullRequest{{Number: 42, HeadRefName: "feat/x", CIStatus: "FAILURE"}},
			tasks: []TaskMatcher{
				{ID: "t1", PRNumber: 42},
				{ID: "t2", Branch: "feat/x"},
			},
			want: []PRIssue{{Kind: PRIssueCIFailure, TaskID: "t1", PR: PullRequest{Number: 42, HeadRefName: "feat/x", CIStatus: "FAILURE"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchTaskPRs(tt.prs, tt.tasks)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Kind != tt.want[i].Kind {
					t.Errorf("issue[%d].Kind = %q, want %q", i, got[i].Kind, tt.want[i].Kind)
				}
				if got[i].TaskID != tt.want[i].TaskID {
					t.Errorf("issue[%d].TaskID = %q, want %q", i, got[i].TaskID, tt.want[i].TaskID)
				}
			}
		})
	}
}

func TestMatchReviewPRs(t *testing.T) {
	tests := []struct {
		name     string
		prs      []PullRequest
		projects []ProjectMatcher
		want     int
	}{
		{
			name:     "no match",
			prs:      []PullRequest{{Repository: "other/repo"}},
			projects: []ProjectMatcher{{ID: "o/r", Repository: "o/r"}},
			want:     0,
		},
		{
			name:     "match by repo",
			prs:      []PullRequest{{Number: 1, Repository: "o/r"}},
			projects: []ProjectMatcher{{ID: "o/r", Repository: "o/r"}},
			want:     1,
		},
		{
			name: "multiple matches",
			prs: []PullRequest{
				{Number: 1, Repository: "o/r"},
				{Number: 2, Repository: "o/r"},
				{Number: 3, Repository: "other/x"},
			},
			projects: []ProjectMatcher{{ID: "o/r", Repository: "o/r"}},
			want:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchReviewPRs(tt.prs, tt.projects)
			if len(got) != tt.want {
				t.Fatalf("got %d matches, want %d", len(got), tt.want)
			}
		})
	}
}
