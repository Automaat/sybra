// Package prreview classifies a task as the review of a pull request Sybra
// did not open, which no path may push to.
package prreview

import "slices"

// Tag is the task tag every pull-request review task carries.
const Tag = "review"

// TagAdopted marks a review-tagged task that owns its pull request: Sybra
// opened it and re-adopted it after losing the original task.
const TagAdopted = "adopted-pr"

// Is reports whether the task reviews a pull request it must never write to.
func Is(prNumber int, tags []string) bool {
	if prNumber <= 0 || !slices.Contains(tags, Tag) {
		return false
	}
	return !slices.Contains(tags, TagAdopted)
}
