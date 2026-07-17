package github

import (
	"fmt"
	"sort"
	"strings"
)

// ClassifyCIFlakiness fetches every check-run attempt for a commit (filter=all,
// unlike the filter=latest rollup used elsewhere) and classifies whether the
// current CI failure is flaky: every gating check that has at least one
// FAILURE on this SHA also has at least one SUCCESS with a same-check success
// rate at or above threshold. allFlaky is false whenever any gating check
// fails on every one of its attempts (a deterministic failure) or the classifier
// finds no evidence either way (fetch/parse error, or no failing check at
// all) — callers must treat those identically to "not flaky" so a classifier
// outage never suppresses a real escalation. flakyChecks names the checks that
// were classified flaky, sorted for deterministic output.
func ClassifyCIFlakiness(repo, sha string, threshold float64) (allFlaky bool, flakyChecks []string, err error) {
	return classifyCIFlakinessWith(defaultExecer, repo, sha, threshold)
}

func classifyCIFlakinessWith(e execer, repo, sha string, threshold float64) (bool, []string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || sha == "" {
		return false, nil, fmt.Errorf("invalid repo or sha: %s@%s", repo, sha)
	}

	runs, fetched := fetchCheckRunsWith(e, owner, name, sha, "all")
	if !fetched {
		return false, nil, fmt.Errorf("fetch check-runs %s@%s: request failed", repo, sha)
	}

	type tally struct {
		success, failure int
	}
	byName := make(map[string]*tally)
	for _, c := range runs.CheckRuns {
		if c.Name == "" || isNonGatingCheck(c.Name) {
			continue
		}
		state := effectiveCheckState(gqlCheckContext{
			Typename:   "CheckRun",
			Status:     strings.ToUpper(c.Status),
			Conclusion: strings.ToUpper(c.Conclusion),
		})
		if state != "SUCCESS" && state != "FAILURE" {
			continue
		}
		t := byName[c.Name]
		if t == nil {
			t = &tally{}
			byName[c.Name] = t
		}
		if state == "SUCCESS" {
			t.success++
		} else {
			t.failure++
		}
	}

	var flaky []string
	sawFailure := false
	for checkName, t := range byName {
		if t.failure == 0 {
			continue
		}
		sawFailure = true
		successRate := float64(t.success) / float64(t.success+t.failure)
		if t.success == 0 || successRate < threshold {
			// A deterministic (or below-threshold) failure means the overall
			// CI failure is not attributable to flakiness alone.
			return false, nil, nil
		}
		flaky = append(flaky, checkName)
	}
	if !sawFailure {
		return false, nil, nil
	}
	sort.Strings(flaky)
	return true, flaky, nil
}
