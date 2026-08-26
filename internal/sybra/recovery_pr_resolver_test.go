package sybra

import (
	"context"
	"errors"
	"strings"
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
		owner    string
		wantHead string
	}{
		{
			name:     "a fork push qualifies the head so the owner filter arms",
			owner:    resolverFork,
			wantHead: resolverFork + ":" + resolverBranch,
		},
		{
			name:     "an origin push qualifies with the upstream account",
			owner:    "upstream",
			wantHead: "upstream:" + resolverBranch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a project that pushes to the named account
			var gotHead string
			rp := recoveryPRResolver{
				projects: stubProjectGetter{},
				pushOwner: func(context.Context, string) (string, error) {
					return tc.owner, nil
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

func TestResolvePRForTask_UnknownHeadOwner(t *testing.T) {
	tests := []struct {
		name     string
		projErr  error
		ownerErr error
	}{
		{name: "an unregistered project", projErr: errors.New("no such project")},
		{name: "a clone with no readable github remote", ownerErr: errors.New("no readable github remote")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given a project whose push target cannot be established
			var searched, lookedUp bool
			rp := recoveryPRResolver{
				projects: stubProjectGetter{err: tc.projErr},
				pushOwner: func(context.Context, string) (string, error) {
					if tc.ownerErr != nil {
						return "", tc.ownerErr
					}
					return resolverFork, nil
				},
				findByBranch: func(_ context.Context, _, _ string) (int, string, bool, error) {
					searched = true
					return 217, "MERGED", true, nil
				},
				issueLinked: func(_ string, _ int) ([]github.PullRequest, error) {
					lookedUp = true
					return []github.PullRequest{{Number: 9, HeadRefName: resolverBranch, HeadRepoOwner: resolverFork}}, nil
				},
			}

			// When the task's PR is resolved
			got, err := rp.ResolvePRForTask(context.Background(), resolverRepo, resolverBranch, resolverIssue)

			// Then neither lookup runs and no PR is adopted
			if err != nil {
				t.Fatalf("ResolvePRForTask: %v", err)
			}
			if got != (recovery.PRRef{}) {
				t.Errorf("ref = %+v, want empty", got)
			}
			if searched || lookedUp {
				t.Errorf("searched = %v, looked up = %v, want neither", searched, lookedUp)
			}
		})
	}
}

func TestResolvePRForTask_BranchSearchFiltersForeignHeads(t *testing.T) {
	tests := []struct {
		name      string
		fork      string
		headOwner string
		state     string
		want      recovery.PRRef
	}{
		{
			name:      "our own fork pr is adopted",
			fork:      resolverFork,
			headOwner: resolverFork,
			state:     "OPEN",
			want:      recovery.PRRef{Number: 217, State: "OPEN"},
		},
		{
			name:      "an outsider fork reusing our branch is refused",
			fork:      "",
			headOwner: "outsider",
			state:     "OPEN",
			want:      recovery.PRRef{},
		},
		{
			name:      "a merged outsider pr never lands the task",
			fork:      "",
			headOwner: "outsider",
			state:     "MERGED",
			want:      recovery.PRRef{},
		},
		{
			name:      "a deleted head repo is refused",
			fork:      resolverFork,
			headOwner: "",
			state:     "OPEN",
			want:      recovery.PRRef{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Given the real head filter over a single candidate PR
			rp := recoveryPRResolver{
				projects: stubProjectGetter{},
				pushOwner: func(context.Context, string) (string, error) {
					return tc.fork, nil
				},
				findByBranch: func(_ context.Context, _, head string) (int, string, bool, error) {
					wantOwner, _ := github.SplitHead(head)
					if wantOwner != "" && !strings.EqualFold(tc.headOwner, wantOwner) {
						return 0, "", false, nil
					}
					return 217, tc.state, true, nil
				},
				issueLinked: func(string, int) ([]github.PullRequest, error) { return nil, nil },
			}

			// When the task's PR is resolved
			got, err := rp.ResolvePRForTask(context.Background(), resolverRepo, resolverBranch, resolverIssue)

			// Then a head repo we do not push to never becomes the task's PR
			if err != nil {
				t.Fatalf("ResolvePRForTask: %v", err)
			}
			if got != tc.want {
				t.Errorf("ref = %+v, want %+v", got, tc.want)
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
			name:   "another task's pr from the same fork is refused",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: "feat/other-task-def456", HeadRepoOwner: resolverFork},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "a deleted fork leaves no head owner to match",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   resolverFork,
			linked: []github.PullRequest{
				{Number: 9, HeadRefName: resolverBranch},
			},
			want:       recovery.PRRef{},
			wantLookup: true,
		},
		{
			name:   "an origin-push project adopts its own upstream branch",
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
			name:   "an origin-push project refuses an outsider fork on our branch",
			branch: resolverBranch,
			issue:  resolverIssue,
			fork:   "upstream",
			linked: []github.PullRequest{
				{Number: 901, HeadRefName: resolverBranch, HeadRepoOwner: "outsider"},
			},
			want:       recovery.PRRef{},
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
				pushOwner: func(context.Context, string) (string, error) {
					return tc.fork, nil
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
			pushOwner:   func(context.Context, string) (string, error) { return "upstream", nil },
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
