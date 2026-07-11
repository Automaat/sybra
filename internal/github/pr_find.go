package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// findPRRunner runs `gh pr list` for FindPRForBranch. A package var so tests
// can inject a fake without a real gh/network — the same seam createPRRunner
// uses. Routes through ghGate + ghEnv like every other gh call in the package,
// so the idempotency lookup shares the same rate-limit pacing and GitHub App
// installation-token identity as the CreatePR call it guards.
var findPRRunner = ghRunCtx

// FindPRForBranch returns the number of an open PR whose head is branch, if
// one exists. head is the `gh pr list --head` value: a bare branch name, or
// "fork-owner:branch" for a fork-hosted branch. found is false (with nil err)
// when no PR matches; a non-nil err signals the lookup itself failed and the
// caller cannot distinguish "no PR" from "could not check".
func FindPRForBranch(ctx context.Context, repo, head string) (number int, found bool, err error) {
	owner, branch := splitHead(head)
	out, runErr := findPRRunner(ctx, "pr", "list",
		"--repo", repo, "--head", branch, "--state", "open",
		"--json", "number,headRepositoryOwner", "--limit", "20")
	if runErr != nil {
		return 0, false, fmt.Errorf("gh pr list --head %s: %s: %w", branch, sanitizeGHOutput(out), runErr)
	}
	var prs []struct {
		Number              int `json:"number"`
		HeadRepositoryOwner struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if jsonErr := json.Unmarshal(out, &prs); jsonErr != nil {
		return 0, false, fmt.Errorf("parse pr list: %w", jsonErr)
	}
	for _, pr := range prs {
		if pr.Number <= 0 {
			continue
		}
		if owner != "" && !strings.EqualFold(pr.HeadRepositoryOwner.Login, owner) {
			continue
		}
		return pr.Number, true, nil
	}
	return 0, false, nil
}

// FindPRForBranchAnyState resolves the PR for a branch across every state so a
// task that lost its pr_number can be reconciled. A MERGED PR wins (the task
// has landed and must advance to done); otherwise a single OPEN PR is returned
// to backfill the number. state is "MERGED" or "OPEN". found is false (nil err)
// when no unambiguous PR matches — several open PRs, or only closed-unmerged
// ones, leave the caller to make no change.
func FindPRForBranchAnyState(ctx context.Context, repo, head string) (number int, state string, found bool, err error) {
	owner, branch := splitHead(head)
	out, runErr := findPRRunner(ctx, "pr", "list",
		"--repo", repo, "--head", branch, "--state", "all",
		"--json", "number,state,headRepositoryOwner", "--limit", "20")
	if runErr != nil {
		return 0, "", false, fmt.Errorf("gh pr list --head %s --state all: %s: %w", branch, sanitizeGHOutput(out), runErr)
	}
	var prs []struct {
		Number              int    `json:"number"`
		State               string `json:"state"`
		HeadRepositoryOwner struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
	}
	if jsonErr := json.Unmarshal(out, &prs); jsonErr != nil {
		return 0, "", false, fmt.Errorf("parse pr list: %w", jsonErr)
	}
	var mergedNum, openNum, openCount int
	for _, pr := range prs {
		if pr.Number <= 0 {
			continue
		}
		if owner != "" && !strings.EqualFold(pr.HeadRepositoryOwner.Login, owner) {
			continue
		}
		switch pr.State {
		case "MERGED":
			if mergedNum == 0 {
				mergedNum = pr.Number
			}
		case "OPEN":
			openCount++
			openNum = pr.Number
		}
	}
	if mergedNum > 0 {
		return mergedNum, "MERGED", true, nil
	}
	if openCount == 1 {
		return openNum, "OPEN", true, nil
	}
	return 0, "", false, nil
}

func splitHead(head string) (owner, branch string) {
	if o, b, found := strings.Cut(head, ":"); found {
		return o, b
	}
	return "", head
}
