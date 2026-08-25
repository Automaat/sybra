// Package prreview classifies a task as the review of a pull request Sybra
// did not open, which no path may push to.
package prreview

import (
	"regexp"
	"slices"
)

// Tag is the task tag every pull-request review task carries.
const Tag = "review"

var mintedBranchRe = regexp.MustCompile(`-([0-9a-f]{8})$`)

// Is reports whether the task reviews a pull request it must never write to.
func Is(taskID, branch string, prNumber int, tags []string) bool {
	if prNumber <= 0 || !slices.Contains(tags, Tag) {
		return false
	}
	return !mintedForAnotherTask(branch, taskID)
}

func mintedForAnotherTask(branch, taskID string) bool {
	m := mintedBranchRe.FindStringSubmatch(branch)
	return len(m) == 2 && m[1] != taskID
}
