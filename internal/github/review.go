package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FetchReviews returns open PRs created by the user and review requests, excluding bots.
func FetchReviews() (ReviewSummary, error) {
	return fetchReviewsWith(defaultExecer)
}

func fetchReviewsWith(e execer) (ReviewSummary, error) {
	var summary ReviewSummary

	created, err := searchPRsWith(e, "is:pr is:open author:@me")
	if err != nil {
		return summary, fmt.Errorf("fetch created PRs: %w", err)
	}
	summary.CreatedByMe = created

	requested, err := searchPRsWith(e, "is:pr is:open review-requested:@me")
	if err != nil {
		return summary, fmt.Errorf("fetch review requests: %w", err)
	}
	summary.ReviewRequested = requested

	return summary, nil
}

// HasPendingReview checks if the authenticated user has a pending (draft) review on a PR.
// Pending reviews are only visible to their author via the REST API.
func HasPendingReview(repo string, number int) (bool, error) {
	return hasPendingReviewWith(defaultExecer, repo, number)
}

func hasPendingReviewWith(e execer, repo string, number int) (bool, error) {
	out, err := e.run("api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number))
	if err != nil {
		return false, fmt.Errorf("fetch reviews for %s#%d: %s: %w", repo, number, strings.TrimSpace(string(out)), err)
	}
	var reviews []struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &reviews); err != nil {
		return false, fmt.Errorf("parse reviews: %w", err)
	}
	for i := range reviews {
		if reviews[i].State == "PENDING" {
			return true, nil
		}
	}
	return false, nil
}

// ApprovePR approves a pull request.
func ApprovePR(repo string, number int) error {
	return approvePRWith(defaultExecer, repo, number)
}

func approvePRWith(e execer, repo string, number int) error {
	out, err := e.run("pr", "review", "--approve",
		fmt.Sprintf("%d", number), "-R", repo)
	if err != nil {
		return fmt.Errorf("gh pr review --approve %d: %s: %w", number, strings.TrimSpace(string(out)), err)
	}
	return nil
}
