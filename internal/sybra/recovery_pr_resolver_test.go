package sybra

import (
	"context"
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
)

const (
	resolverRepo   = "upstream/app"
	resolverIssue  = "https://github.com/upstream/app/issues/42"
	resolverBranch = "feat/thing-abc123"
	resolverFork   = "sybrabot"
)

type stubProjectGetter struct {
	err error
}

func (s stubProjectGetter) Get(id string) (project.Project, error) {
	if s.err != nil {
		return project.Project{}, s.err
	}
	return project.Project{ID: id, ClonePath: "/clones/" + id}, nil
}

func TestResolvePRForTask_HeadSearch(t *testing.T) {
	tests := []struct {
		name     string
		fork     string
		headErr  error
		projErr  error
		wantHead string
	}{
		{
			name:     "fork push qualifies the head so the owner filter arms",
			fork:     resolverFork,
			wantHead: resolverFork + ":" + resolverBranch,
		},
		{
			name:     "pushing to the upstream repo leaves the head bare",
			fork:     "upstream",
			wantHead: resolverBranch,
		},
		{
			name:     "an unresolvable head falls back to the bare branch",
			headErr:  errors.New("no fork remote"),
			wantHead: resolverBranch,
		},
		{
			name:     "an unknown project falls back to the bare branch",
			projErr:  errors.New("no such project"),
			wantHead: resolverBranch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a resolver whose project resolves to the named fork owner
			var gotHead string
			rp := recoveryPRResolver{
				projects: stubProjectGetter{err: tc.projErr},
				headArg: func(_ context.Context, _, branch string) (string, error) {
					if tc.headErr != nil {
						return "", tc.headErr
					}
					if tc.fork == "" {
						return branch, nil
					}
					return tc.fork + ":" + branch, nil
				},
				findByBranch: func(_ context.Context, _, head string) (int, string, bool, error) {
					gotHead = head
					return 5, "OPEN", true, nil
				},
				issueLinked: func(string, int) ([]github.PullRequest, error) { return nil, nil },
			}

			// When the task's PR is resolved
			got, err := rp.ResolvePRForTask(context.Background(), resolverRepo, resolverBranch, resolverIssue)

			// Then the head search runs against the expected ref
			if err != nil {
				t.Fatalf("ResolvePRForTask: %v", err)
			}
			if gotHead != tc.wantHead {
				t.Errorf("head = %q, want %q", gotHead, tc.wantHead)
			}
			if want := (recovery.PRRef{Number: 5, State: "OPEN"}); got != want {
				t.Errorf("ref = %+v, want %+v", got, want)
			}
		})
	}
}

func TestResolvePRForTask_IssueFallback(t *testing.T) {
	tests := []struct {
		name       string
		branch     string
		issue      string
		fork       string
		linked     []github.PullRequest
		want       recovery.PRRef
		wantLookup bool
	}{
		{
			name:   "a linked pr from our fork on our branch is adopted",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork},
			},
			want:       recovery.PRRef{Number: 9, State: "OPEN"},
			wantLookup: true,
		},
		{
			name:   "a stranger's pr on the same issue is refused",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 16571, HeadRefName: "fix/their-own-branch", HeadRepoOwner: "stranger"},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "a stranger's pr that reuses our branch name is refused",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 16571, HeadRefName: resolverBranch, HeadRepoOwner: "stranger"},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "our single fork pr is adopted after the branch was renamed",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: "feat/older-name-abc123", HeadRepoOwner: resolverFork},
				{Number: 16571, HeadRefName: "fix/their-own-branch", HeadRepoOwner: "stranger"},
			},
			want:       recovery.PRRef{Number: 9, State: "OPEN"},
			wantLookup: true,
		},
		{
			name:   "two of our fork prs on other branches stay ambiguous",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: "feat/older-name-abc123", HeadRepoOwner: resolverFork},
				{Number: 10, HeadRefName: "feat/other-name-abc123", HeadRepoOwner: resolverFork},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "without a fork only an exact branch match is adopted",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   "upstream",
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: "feat/older-name-abc123", HeadRepoOwner: "upstream"},
				{Number: 10, HeadRefName: resolverBranch, HeadRepoOwner: "upstream"},
			},
			want:       recovery.PRRef{Number: 10, State: "OPEN"},
			wantLookup: true,
		},
		{
			name:   "two linked prs on our branch stay ambiguous",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork},
				{Number: 10, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "an issue in another repo is not consulted",
			branch: resolverBranch,
			issue:  "https://github.com/elsewhere/app/issues/42",
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork},
			},
			want: recovery.PRRef{},
		},
		{
			name:   "no branch means no lookup at all",
			branch: "",
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork},
			},
			want: recovery.PRRef{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a head search that finds nothing
			var lookedUp bool
			rp := recoveryPRResolver{
				projects: stubProjectGetter{},
				headArg: func(_ context.Context, _, branch string) (string, error) {
					return tc.fork + ":" + branch, nil
				},
				findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
					return 0, "", false, nil
				},
				issueLinked: func(_ string, _ int) ([]github.PullRequest, error) {
					lookedUp = true
					return tc.linked, nil
				},
			}

			// When the task's PR is resolved
			got, err := rp.ResolvePRForTask(context.Background(), resolverRepo, tc.branch, tc.issue)

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
	// Given each lookup failing in turn
	wantErr := errors.New("boom")
	base := func() recoveryPRResolver {
		return recoveryPRResolver{
			projects:    stubProjectGetter{},
			headArg:     func(_ context.Context, _, branch string) (string, error) { return branch, nil },
			issueLinked: func(string, int) ([]github.PullRequest, error) { return nil, nil },
			findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
				return 0, "", false, nil
			},
		}
	}
	branchErr := base()
	branchErr.findByBranch = func(_ context.Context, _, _ string) (int, string, bool, error) {
		return 0, "", false, wantErr
	}
	linkErr := base()
	linkErr.issueLinked = func(string, int) ([]github.PullRequest, error) { return nil, wantErr }

	// When each resolver runs
	// Then the error reaches the caller instead of an empty ref
	for name, rp := range map[string]recoveryPRResolver{"branch": branchErr, "linked": linkErr} {
		t.Run(name, func(t *testing.T) {
			_, err := rp.ResolvePRForTask(context.Background(), resolverRepo, resolverBranch, resolverIssue)
			if !errors.Is(err, wantErr) {
				t.Fatalf("err = %v, want %v", err, wantErr)
			}
		})
	}
}
