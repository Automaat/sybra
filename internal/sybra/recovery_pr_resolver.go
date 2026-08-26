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
	headArg      func(ctx context.Context, worktreePath, branch string) (string, error)
	projects     recovery.ProjectGetter
}

func newRecoveryPRResolver(projects recovery.ProjectGetter) recoveryPRResolver {
	return recoveryPRResolver{
		findByBranch: github.FindPRForBranchAnyState,
		issueLinked:  github.FetchIssueLinkedPRs,
		headArg:      project.HeadArg,
		projects:     projects,
	}
}

func (rp recoveryPRResolver) ResolvePRForTask(ctx context.Context, repo, branch, issue string) (recovery.PRRef, error) {
	if branch == "" {
		return recovery.PRRef{}, nil
	}
	head, owner := rp.qualifyHead(ctx, repo, branch)
	num, state, found, err := rp.findByBranch(ctx, repo, head)
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
	return ownedLinkedPR(prs, owner, branch), nil
}

func (rp recoveryPRResolver) qualifyHead(ctx context.Context, repo, branch string) (head, owner string) {
	if rp.projects == nil || rp.headArg == nil {
		return branch, ""
	}
	proj, err := rp.projects.Get(repo)
	if err != nil {
		return branch, ""
	}
	head, err = rp.headArg(ctx, proj.ClonePath, branch)
	if err != nil || head == "" {
		return branch, ""
	}
	owner, _ = github.SplitHead(head)
	if repoOwner, _, _ := strings.Cut(repo, "/"); strings.EqualFold(owner, repoOwner) {
		return branch, ""
	}
	return head, owner
}

func ownedLinkedPR(prs []github.PullRequest, forkOwner, branch string) recovery.PRRef {
	var owned, exact []github.PullRequest
	for i := range prs {
		if prs[i].Number <= 0 || !ownsHeadRepo(prs[i], forkOwner) {
			continue
		}
		owned = append(owned, prs[i])
		if prs[i].HeadRefName == branch {
			exact = append(exact, prs[i])
		}
	}
	switch {
	case len(exact) == 1:
		return recovery.PRRef{Number: exact[0].Number, State: "OPEN"}
	case len(exact) == 0 && forkOwner != "" && len(owned) == 1:
		return recovery.PRRef{Number: owned[0].Number, State: "OPEN"}
	}
	return recovery.PRRef{}
}

func ownsHeadRepo(pr github.PullRequest, forkOwner string) bool {
	return forkOwner == "" || strings.EqualFold(pr.HeadRepoOwner, forkOwner)
}
