package sybra

import (
	"context"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/recovery"
)

type recoveryPRResolver struct {
	findByBranch func(ctx context.Context, repo, head string) (int, string, bool, error)
	issueLinked  func(repo string, issueNumber int) ([]github.PullRequest, error)
}

func newRecoveryPRResolver() recoveryPRResolver {
	return recoveryPRResolver{
		findByBranch: github.FindPRForBranchAnyState,
		issueLinked:  github.FetchIssueLinkedPRs,
	}
}

func (rp recoveryPRResolver) ResolvePRForTask(ctx context.Context, repo, branch, issue string) (recovery.PRRef, error) {
	if branch == "" {
		return recovery.PRRef{}, nil
	}
	num, state, found, err := rp.findByBranch(ctx, repo, branch)
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
	return ownedLinkedPR(prs, branch), nil
}

func ownedLinkedPR(prs []github.PullRequest, branch string) recovery.PRRef {
	_, want := github.SplitHead(branch)
	match := recovery.PRRef{}
	for i := range prs {
		if prs[i].Number <= 0 || prs[i].HeadRefName != want {
			continue
		}
		if match.Number > 0 {
			return recovery.PRRef{}
		}
		match = recovery.PRRef{Number: prs[i].Number, State: "OPEN"}
	}
	return match
}
