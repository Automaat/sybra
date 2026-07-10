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
	if branch != "" {
		num, state, found, err := rp.findByBranch(ctx, repo, branch)
		if err != nil {
			return recovery.PRRef{}, err
		}
		if found {
			return recovery.PRRef{Number: num, State: state}, nil
		}
	}
	if issue != "" {
		if _, number := github.ParseIssueURL(issue); number > 0 {
			prs, err := rp.issueLinked(repo, number)
			if err != nil {
				return recovery.PRRef{}, err
			}
			if len(prs) == 1 && prs[0].Number > 0 {
				return recovery.PRRef{Number: prs[0].Number, State: "OPEN"}, nil
			}
		}
	}
	return recovery.PRRef{}, nil
}
