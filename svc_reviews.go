package main

import (
	"fmt"

	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/task"
)

// ReviewService exposes PR review operations as Wails-bound methods.
type ReviewService struct {
	reviewer *ReviewHandler
	tasks    *task.Manager
}

// StartReview starts a headless review agent for the task's linked PR.
func (s *ReviewService) StartReview(taskID string) error {
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return err
	}
	if t.ProjectID == "" || t.PRNumber == 0 {
		return fmt.Errorf("task %s has no linked PR", taskID)
	}
	return s.reviewer.startReviewAgent(t)
}

// StartFixReview starts a headless fix-review agent that applies unresolved
// PR review comment fixes for the task's linked PR.
func (s *ReviewService) StartFixReview(taskID string) error {
	t, err := s.tasks.Get(taskID)
	if err != nil {
		return err
	}
	return s.reviewer.startFixReviewAgent(t)
}

// ListReviewComments returns all review comments for a task.
func (s *ReviewService) ListReviewComments(taskID string) ([]task.ReviewComment, error) {
	return s.tasks.Comments().List(taskID)
}

// AddReviewComment adds a review comment at the given line.
func (s *ReviewService) AddReviewComment(taskID string, line int, body string) (task.ReviewComment, error) {
	return s.tasks.Comments().Add(taskID, line, body)
}

// ResolveReviewComment marks a comment as resolved.
func (s *ReviewService) ResolveReviewComment(taskID, commentID string) error {
	return s.tasks.Comments().Resolve(taskID, commentID)
}

// DeleteReviewComment removes a review comment.
func (s *ReviewService) DeleteReviewComment(taskID, commentID string) error {
	return s.tasks.Comments().Delete(taskID, commentID)
}

// FetchReviews fetches the current PR review summary from GitHub.
func (s *ReviewService) FetchReviews() (github.ReviewSummary, error) {
	return github.FetchReviews()
}

// MarkPRReady converts a PR from draft to ready-for-review.
func (s *ReviewService) MarkPRReady(repo string, number int) error {
	return github.MarkReady(repo, number)
}
