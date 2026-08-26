package sybra

import (
	"context"
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/recovery"
)

func TestResolvePRForTask(t *testing.T) {
	const (
		repo  = "upstream/app"
		issue = "https://github.com/upstream/app/issues/42"
		mine  = "feat/thing-abc123"
	)

	tests := []struct {
		name        string
		branch      string
		issue       string
		branchFound bool
		branchNum   int
		branchState string
		linked      []github.PullRequest
		want        recovery.PRRef
		wantLookup  bool
	}{
		{
			name:        "head search wins",
			branch:      mine,
			issue:       issue,
			branchFound: true,
			branchNum:   7,
			branchState: "OPEN",
			want:        recovery.PRRef{Number: 7, State: "OPEN"},
		},
		{
			name:   "linked pr on the task branch is adopted",
			branch: mine,
			issue:  issue,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: mine},
			},
			want:       recovery.PRRef{Number: 9, State: "OPEN"},
			wantLookup: true,
		},
		{
			name:   "linked pr from another contributor is refused",
			branch: mine,
			issue:  issue,
			linked: []github.PullRequest{
				{Number: 16571, HeadRefName: "fix/their-own-branch"},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "two linked prs on the same branch stay ambiguous",
			branch: mine,
			issue:  issue,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: mine},
				{Number: 10, HeadRefName: mine},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "fork-qualified task branch matches the bare head ref",
			branch: "contributor:" + mine,
			issue:  issue,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: mine},
			},
			want:       recovery.PRRef{Number: 9, State: "OPEN"},
			wantLookup: true,
		},
		{
			name:   "no branch means no lookup at all",
			branch: "",
			issue:  issue,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: mine},
			},
			want: recovery.PRRef{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a resolver whose two lookups are stubbed
			var lookedUp bool
			rp := recoveryPRResolver{
				findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
					return tc.branchNum, tc.branchState, tc.branchFound, nil
				},
				issueLinked: func(_ string, _ int) ([]github.PullRequest, error) {
					lookedUp = true
					return tc.linked, nil
				},
			}

			// When the task's PR is resolved
			got, err := rp.ResolvePRForTask(context.Background(), repo, tc.branch, tc.issue)

			// Then only a PR the task pushed comes back
			if err != nil {
				t.Fatalf("ResolvePRForTask: %v", err)
			}
			if got != tc.want {
				t.Errorf("ref = %+v, want %+v", got, tc.want)
			}
			if lookedUp != tc.wantLookup {
				t.Errorf("issue lookup ran = %v, want %v", lookedUp, tc.wantLookup)
			}
		})
	}
}

func TestResolvePRForTask_PropagatesLookupErrors(t *testing.T) {
	// Given both lookups failing
	wantErr := errors.New("boom")
	branchErr := recoveryPRResolver{
		findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
			return 0, "", false, wantErr
		},
		issueLinked: func(_ string, _ int) ([]github.PullRequest, error) { return nil, nil },
	}
	linkErr := recoveryPRResolver{
		findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
			return 0, "", false, nil
		},
		issueLinked: func(_ string, _ int) ([]github.PullRequest, error) { return nil, wantErr },
	}

	// When each resolver runs
	// Then the error reaches the caller instead of an empty ref
	for name, rp := range map[string]recoveryPRResolver{"branch": branchErr, "linked": linkErr} {
		t.Run(name, func(t *testing.T) {
			_, err := rp.ResolvePRForTask(context.Background(), "upstream/app", "feat/x", "https://github.com/upstream/app/issues/42")
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
		})
	}
}
