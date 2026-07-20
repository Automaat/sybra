package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// RerunFailedChecks reruns the latest failed gating workflow run for a PR.
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
		"--json", "databaseId,conclusion,headSha,name,workflowName")
	if err != nil {
		return 0, fmt.Errorf("gh run list failed commit %q: %s: %w", headSHA, strings.TrimSpace(string(out)), err)
	}
	var runs []struct {
		DatabaseID   int    `json:"databaseId"`
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"headSha"`
		Name         string `json:"name"`
		WorkflowName string `json:"workflowName"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return 0, fmt.Errorf("parse failed run list: %w", err)
	}
	var fallback int
	for _, run := range runs {
		if run.HeadSHA != "" && !strings.EqualFold(run.HeadSHA, headSHA) {
			continue
		}
		if run.DatabaseID == 0 || !isBlockingCheckRunConclusion(run.Conclusion) {
			continue
		}
		if isNonGatingWorkflowRun(run.Name, run.WorkflowName) {
			continue
		}
		hasGatingJob, err := hasBlockingGatingJobWith(e, repo, run.DatabaseID)
		if err != nil {
			if fallback == 0 {
				fallback = run.DatabaseID
			}
			continue
		}
		if hasGatingJob {
			return run.DatabaseID, nil
		}
	}
	if fallback != 0 {
		return fallback, nil
	}
	return 0, fmt.Errorf("gh run list failed commit %q: no failed gating workflow runs found", headSHA)
}

func hasBlockingGatingJobWith(e execer, repo string, runID int) (bool, error) {
	out, err := e.run("run", "view", strconv.Itoa(runID),
		"--repo", repo,
		"--json", "jobs")
	if err != nil {
		return false, fmt.Errorf("gh run view %d jobs: %s: %w", runID, strings.TrimSpace(string(out)), err)
	}
	var raw struct {
		Jobs []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return false, fmt.Errorf("parse run jobs: %w", err)
	}
	for _, job := range raw.Jobs {
		if isNonGatingCheck(job.Name) {
			continue
		}
		if isBlockingCheckRunConclusion(job.Conclusion) {
			return true, nil
		}
	}
	return false, nil
}

func isNonGatingWorkflowRun(name, workflowName string) bool {
	return isNonGatingCheck(name) || isNonGatingCheck(workflowName)
}
