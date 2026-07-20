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
	headSHA, err := fetchPRHeadSHAWith(nil, e, repo, number)
	if err != nil {
		return err
	}
	runID, err := latestFailedRunIDOnCommitWith(e, repo, headSHA)
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

func latestFailedRunIDOnCommitWith(e execer, repo, headSHA string) (int, error) {
	out, err := e.run("run", "list",
		"--repo", repo,
		"--commit", headSHA,
		"--limit", "50",
		"--json", "databaseId,conclusion,headSha")
	if err != nil {
		return 0, fmt.Errorf("gh run list failed commit %q: %s: %w", headSHA, strings.TrimSpace(string(out)), err)
	}
	var runs []struct {
		DatabaseID int    `json:"databaseId"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"headSha"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return 0, fmt.Errorf("parse failed run list: %w", err)
	}
	for _, run := range runs {
		if run.HeadSHA != "" && !strings.EqualFold(run.HeadSHA, headSHA) {
			continue
		}
		if run.DatabaseID != 0 && isBlockingCheckRunConclusion(run.Conclusion) {
			return run.DatabaseID, nil
		}
	}
	return 0, fmt.Errorf("gh run list failed commit %q: no failed workflow runs found", headSHA)
}
