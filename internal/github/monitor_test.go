package github

import (
	"fmt"
	"testing"
)

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

func TestDetectClosedTaskPRs(t *testing.T) {
	merged := PRState{State: "MERGED", MergedAt: "2026-04-01T12:00:00Z"}
	closed := PRState{State: "CLOSED"}
	open := PRState{State: "OPEN"}

	makeFetch := func(states map[int]PRState) func(string, int) (PRState, error) {
		return func(_ string, num int) (PRState, error) {
			if s, ok := states[num]; ok {
				return s, nil
			}
			return PRState{}, fmt.Errorf("not found")
		}
	}

	tests := []struct {
		name    string
		openPRs []PullRequest
		tasks   []TaskMatcher
		states  map[int]PRState
		want    []ClosedPR
	}{
		{
			name:    "no tasks",
			openPRs: nil,
			tasks:   nil,
			states:  nil,
			want:    nil,
		},
		{
			name:    "task without PRNumber skipped",
			openPRs: nil,
			tasks:   []TaskMatcher{{ID: "t1", Branch: "feat/x", ProjectID: "o/r"}},
			states:  nil,
			want:    nil,
		},
		{
			name:    "task without ProjectID skipped",
			openPRs: nil,
			tasks:   []TaskMatcher{{ID: "t1", PRNumber: 42}},
			states:  map[int]PRState{42: merged},
			want:    nil,
		},
		{
			name:    "PR still open – skipped",
			openPRs: []PullRequest{{Number: 42}},
			tasks:   []TaskMatcher{{ID: "t1", PRNumber: 42, ProjectID: "o/r"}},
			states:  map[int]PRState{42: open},
			want:    nil,
		},
		{
			name:    "PR merged → done",
			openPRs: nil,
			tasks:   []TaskMatcher{{ID: "t1", PRNumber: 42, ProjectID: "o/r"}},
			states:  map[int]PRState{42: merged},
			want:    []ClosedPR{{TaskID: "t1", PRNumber: 42, State: "MERGED"}},
		},
		{
			name:    "PR closed → done",
			openPRs: nil,
			tasks:   []TaskMatcher{{ID: "t1", PRNumber: 42, ProjectID: "o/r"}},
			states:  map[int]PRState{42: closed},
			want:    []ClosedPR{{TaskID: "t1", PRNumber: 42, State: "CLOSED"}},
		},
		{
			name:    "fetch error – skipped",
			openPRs: nil,
			tasks:   []TaskMatcher{{ID: "t1", PRNumber: 99, ProjectID: "o/r"}},
			states:  map[int]PRState{},
			want:    nil,
		},
		{
			name:    "mixed: one open, one merged",
			openPRs: []PullRequest{{Number: 10}},
			tasks: []TaskMatcher{
				{ID: "t1", PRNumber: 10, ProjectID: "o/r"},
				{ID: "t2", PRNumber: 20, ProjectID: "o/r"},
			},
			states: map[int]PRState{10: open, 20: merged},
			want:   []ClosedPR{{TaskID: "t2", PRNumber: 20, State: "MERGED"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectClosedTaskPRs(tt.openPRs, tt.tasks, makeFetch(tt.states))
			if len(got) != len(tt.want) {
				t.Fatalf("got %d results, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].TaskID != tt.want[i].TaskID {
					t.Errorf("[%d] TaskID = %q, want %q", i, got[i].TaskID, tt.want[i].TaskID)
				}
				if got[i].PRNumber != tt.want[i].PRNumber {
					t.Errorf("[%d] PRNumber = %d, want %d", i, got[i].PRNumber, tt.want[i].PRNumber)
				}
				if got[i].State != tt.want[i].State {
					t.Errorf("[%d] State = %q, want %q", i, got[i].State, tt.want[i].State)
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
