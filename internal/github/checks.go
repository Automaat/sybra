package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// RerunFailedChecks reruns the latest failed workflow run for a PR.
func RerunFailedChecks(repo string, number int) error {
	return rerunFailedChecksWith(defaultExecer, repo, number)
}

func rerunFailedChecksWith(e execer, repo string, number int) error {
	branch, err := fetchPRBranchWith(e, repo, number)
	if err != nil {
		return err
	}
	runID, err := latestFailedRunIDOnBranchWith(e, repo, branch)
	if err != nil {
		return err
	}
	out, err := e.run("run", "rerun", strconv.Itoa(runID), "--failed",
		"--repo", repo)
	if err != nil {
		return fmt.Errorf("gh run rerun %d --failed: %s: %w", runID, strings.TrimSpace(string(out)), err)
	}
	if runtimeCacheEnabled(e) {
		invalidatePRCaches(repo, number)
	}
	return nil
}

func latestFailedRunIDOnBranchWith(e execer, repo, branch string) (int, error) {
	out, err := e.run("run", "list",
		"--repo", repo,
		"--branch", branch,
		"--status", "failure",
		"--limit", "1",
		"--json", "databaseId")
	if err != nil {
		return 0, fmt.Errorf("gh run list failed branch %q: %s: %w", branch, strings.TrimSpace(string(out)), err)
	}
	var runs []struct {
		DatabaseID int `json:"databaseId"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return 0, fmt.Errorf("parse failed run list: %w", err)
	}
	if len(runs) == 0 || runs[0].DatabaseID == 0 {
		return 0, fmt.Errorf("gh run list failed branch %q: no failed workflow runs found", branch)
	}
	return runs[0].DatabaseID, nil
}
