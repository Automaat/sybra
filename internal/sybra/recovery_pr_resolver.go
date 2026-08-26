package sybra

import (
	"context"
	"strings"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/recovery"
)

type recoveryPRResolver struct {
	findByBranch func(ctx context.Context, repo, head string) (int, string, bool, error)
	issueLinked  func(repo string, issueNumber int) ([]github.PullRequest, error)
	pushOwner    func(ctx context.Context, repoPath string) (string, error)
	projects     recovery.ProjectGetter
}

func newRecoveryPRResolver(projects recovery.ProjectGetter) recoveryPRResolver {
	return recoveryPRResolver{
		findByBranch: github.FindPRForBranchAnyState,
		issueLinked:  github.FetchIssueLinkedPRs,
		pushOwner:    project.PushOwner,
		projects:     projects,
	}
}

func (rp recoveryPRResolver) ResolvePRForTask(ctx context.Context, repo, branch, issue string) (recovery.PRRef, error) {
	if branch == "" {
		return recovery.PRRef{}, nil
	}
	wantOwner := rp.headOwner(ctx, repo)
	if wantOwner == "" {
		return recovery.PRRef{}, nil
	}
	num, state, found, err := rp.findByBranch(ctx, repo, wantOwner+":"+branch)
	if err != nil {
		return recovery.PRRef{}, err
	}
	if found {
		return recovery.PRRef{Number: num, State: state}, nil
	}
	if issue == "" {
		return recovery.PRRef{}, nil
	}
	parsedRepo, number := github.ParseIssueURL(issue)
	if number <= 0 || parsedRepo != repo {
		return recovery.PRRef{}, nil
	}
	prs, err := rp.issueLinked(repo, number)
	if err != nil {
		return recovery.PRRef{}, err
	}
	return ownedLinkedPR(prs, wantOwner, branch), nil
}

func (rp recoveryPRResolver) headOwner(ctx context.Context, repo string) string {
	if rp.projects == nil || rp.pushOwner == nil {
		return ""
	}
	proj, err := rp.projects.Get(repo)
	if err != nil {
		return ""
	}
	owner, err := rp.pushOwner(ctx, proj.ClonePath)
	if err != nil {
		return ""
	}
	return owner
}

func ownedLinkedPR(prs []github.PullRequest, wantOwner, branch string) recovery.PRRef {
	match := recovery.PRRef{}
	for i := range prs {
		pr := &prs[i]
		if pr.Number <= 0 || pr.HeadRefName != branch || !strings.EqualFold(pr.HeadRepoOwner, wantOwner) {
			continue
		}
		if match.Number > 0 {
			return recovery.PRRef{}
		}
		match = recovery.PRRef{Number: pr.Number, State: "OPEN"}
	}
	return match
}
