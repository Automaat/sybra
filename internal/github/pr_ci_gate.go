package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// FetchPRCIGate reads the PR head and its check rollup in one uncached query.
// Unlike a generic green rollup, absent/skipped required checks never pass.
// The caller must compare SHA with the revision it is authorizing to merge.
func FetchPRCIGate(ctx context.Context, repo string, number int, required []string) (CommitGate, error) {
	return fetchPRCIGateWith(ctx, defaultExecer, repo, number, required)
}

func fetchPRCIGateWith(ctx context.Context, e execer, repo string, number int, required []string) (CommitGate, error) {
	required = NormalizeRequiredChecks(required)
	if number <= 0 || len(required) == 0 {
		return CommitGate{}, fmt.Errorf("CI verification requires a PR and explicit required checks")
	}
	out, err := runE(ctx, e, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", "state,headRefOid,statusCheckRollup")
	if err != nil {
		return CommitGate{}, fmt.Errorf("fetch PR CI: %s: %w", sanitizeGHOutput(out), err)
	}
	var snapshot struct {
		State  string            `json:"state"`
		Head   string            `json:"headRefOid"`
		Checks []gqlCheckContext `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &snapshot); err != nil {
		return CommitGate{}, fmt.Errorf("parse PR CI: %w", err)
	}
	if snapshot.State != "OPEN" || strings.TrimSpace(snapshot.Head) == "" {
		return CommitGate{}, fmt.Errorf("CI verification requires an open PR with a resolved head")
	}
	states := make(map[string]string, len(required))
	for _, name := range required {
		states[name] = ""
	}
	for i := range snapshot.Checks {
		check := &snapshot.Checks[i]
		name := check.effectiveName()
		if _, required := states[name]; !required {
			continue
		}
		state := strictCommitGateState(*check)
		// The deployment commit gate also accepts GitHub's neutral/skipped
		// conclusions. A required factory verification must actually run.
		if check.Typename == "CheckRun" && (check.Conclusion == "SKIPPED" || check.Conclusion == "NEUTRAL") {
			state = "FAILURE"
		}
		if statePriority(state) > statePriority(states[name]) {
			states[name] = state
		}
	}
	return commitGateFromStates(repo, snapshot.Head, required, states), nil
}
