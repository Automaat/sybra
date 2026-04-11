package github

import (
	"fmt"
	"strings"
)

// RerunFailedChecks reruns the latest failed workflow run for a PR.
func RerunFailedChecks(repo string, number int) error {
	return rerunFailedChecksWith(defaultExecer, repo, number)
}

func rerunFailedChecksWith(e execer, repo string, number int) error {
	// Get the PR branch to find the latest run
	branch, err := fetchPRBranchWith(e, repo, number)
	if err != nil {
		return err
	}
	out, err := e.run("run", "rerun", "--failed",
		"--repo", repo, "--branch", branch)
	if err != nil {
		return fmt.Errorf("gh run rerun --failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
