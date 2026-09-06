package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// CommitGate summarizes the state of a commit's required checks.
type CommitGate struct {
	Repo      string            `json:"repo"`
	SHA       string            `json:"sha"`
	Checks    map[string]string `json:"checks"` // required check -> SUCCESS|FAILURE|PENDING
	Missing   []string          `json:"missing"`
	Pending   []string          `json:"pending"`
	Failed    []string          `json:"failed"`
	Succeeded []string          `json:"succeeded"`
}

// Approved reports whether every required check is present and successful.
func (g CommitGate) Approved() bool {
	return len(g.Missing) == 0 && len(g.Pending) == 0 && len(g.Failed) == 0
}

// FetchCommitGate resolves the exact state of the required checks for a commit.
// A CI source may be unavailable under narrow GitHub App permissions; the gate
// only accepts that when another source has resolved every required check.
func FetchCommitGate(ctx context.Context, repo, sha string, requiredChecks []string) (CommitGate, error) {
	return fetchCommitGateWith(ctx, defaultExecer, repo, sha, requiredChecks)
}

func fetchCommitGateWith(ctx context.Context, e execer, repo, sha string, requiredChecks []string) (CommitGate, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.TrimSpace(sha) == "" {
		return CommitGate{}, fmt.Errorf("invalid repo or sha: %s@%s", repo, sha)
	}
	required := NormalizeRequiredChecks(requiredChecks)
	if len(required) == 0 {
		return CommitGate{}, fmt.Errorf("required checks are empty")
	}

	statuses := make(map[string]string, len(required))
	for _, name := range required {
		statuses[name] = ""
	}

	checkRunsErr := error(nil)
	if runs, fetched := fetchCheckRunsCtxWith(ctx, e, owner, name, sha, ""); !fetched {
		checkRunsErr = fmt.Errorf("fetch check-runs %s@%s: request failed", repo, sha)
	} else {
		for _, run := range runs.CheckRuns {
			checkName := strings.TrimSpace(run.Name)
			if _, want := statuses[checkName]; !want {
				continue
			}
			state := strictCommitGateState(gqlCheckContext{
				Typename:   "CheckRun",
				Status:     strings.ToUpper(run.Status),
				Conclusion: strings.ToUpper(run.Conclusion),
			})
			if state != "" {
				statuses[checkName] = state
			}
		}
	}

	resp, err := runGHAPICtxWith(ctx, e, "30s", fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, name, sha))
	if err != nil {
		err = fmt.Errorf("fetch commit statuses %s@%s: %s: %w", repo, sha, sanitizeGHOutput(resp.body), err)
	} else {
		var raw restCommitStatuses
		if err = json.Unmarshal(resp.body, &raw); err != nil {
			err = fmt.Errorf("parse commit statuses %s@%s: %w", repo, sha, err)
		} else {
			seenStatus := make(map[string]bool, len(raw.Statuses))
			for _, status := range raw.Statuses {
				checkName := strings.TrimSpace(status.Context)
				if seenStatus[checkName] {
					continue
				}
				seenStatus[checkName] = true
				if _, want := statuses[checkName]; !want {
					continue
				}
				state := strictCommitGateState(gqlCheckContext{
					Typename: "StatusContext",
					State:    strings.ToUpper(status.State),
				})
				if state != "" && statePriority(state) > statePriority(statuses[checkName]) {
					statuses[checkName] = state
				}
			}
		}
	}
	if checkRunsErr != nil && err != nil {
		return CommitGate{}, fmt.Errorf("%w; %w", checkRunsErr, err)
	}
	if !allRequiredChecksResolved(required, statuses) {
		switch {
		case checkRunsErr != nil:
			return CommitGate{}, checkRunsErr
		case err != nil:
			return CommitGate{}, err
		}
	}

	return commitGateFromStates(repo, sha, required, statuses), nil
}

func commitGateFromStates(repo, sha string, required []string, statuses map[string]string) CommitGate {
	gate := CommitGate{
		Repo:   repo,
		SHA:    sha,
		Checks: statuses,
	}
	for _, checkName := range required {
		switch statuses[checkName] {
		case "SUCCESS":
			gate.Succeeded = append(gate.Succeeded, checkName)
		case "PENDING":
			gate.Pending = append(gate.Pending, checkName)
		case "FAILURE":
			gate.Failed = append(gate.Failed, checkName)
		default:
			gate.Missing = append(gate.Missing, checkName)
		}
	}
	sort.Strings(gate.Missing)
	sort.Strings(gate.Pending)
	sort.Strings(gate.Failed)
	sort.Strings(gate.Succeeded)
	return gate
}

func NormalizeRequiredChecks(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || slices.Contains(out, item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func allRequiredChecksResolved(required []string, statuses map[string]string) bool {
	for _, checkName := range required {
		if statuses[checkName] == "" {
			return false
		}
	}
	return true
}

func statePriority(state string) int {
	switch state {
	case "FAILURE":
		return 3
	case "PENDING":
		return 2
	case "SUCCESS":
		return 1
	default:
		return 0
	}
}

func strictCommitGateState(c gqlCheckContext) string {
	switch c.Typename {
	case "CheckRun":
		if c.Status != "" && c.Status != "COMPLETED" {
			return "PENDING"
		}
		switch strings.ToUpper(c.Conclusion) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			return "SUCCESS"
		}
		return "FAILURE"
	case "StatusContext":
		switch strings.ToUpper(c.State) {
		case "SUCCESS":
			return "SUCCESS"
		case "PENDING", "EXPECTED":
			return "PENDING"
		case "FAILURE", "ERROR":
			return "FAILURE"
		default:
			return "FAILURE"
		}
	default:
		if c.Status != "" || c.Conclusion != "" {
			c.Typename = "CheckRun"
			return strictCommitGateState(c)
		}
		if c.State != "" {
			c.Typename = "StatusContext"
			return strictCommitGateState(c)
		}
		return "FAILURE"
	}
}
