package monitor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DeterministicIssueBody renders the issue body the IssueSink files for
// anomalies that don't require LLM judgment (over_dispatch_limit,
// lost_agent, untriaged). The text is intentionally small and stable so
// dedup by title is meaningful.
func DeterministicIssueBody(a Anomaly) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Detection\n")
	fmt.Fprintf(&b, "- Kind: `%s`\n", a.Kind)
	fmt.Fprintf(&b, "- Severity: `%s`\n", a.Severity)
	fmt.Fprintf(&b, "- Detected at: %s\n", a.DetectedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Fingerprint: `%s`\n\n", a.Fingerprint)
	if a.TaskID != "" {
		fmt.Fprintf(&b, "## Affected task\n- `%s`\n\n", a.TaskID)
	}
	if len(a.Evidence) > 0 {
		fmt.Fprintf(&b, "## Evidence\n```json\n%s\n```\n\n", evidenceJSON(a.Evidence))
	}
	fmt.Fprintf(&b, "## Suggested investigation\n%s\n", suggestedInvestigation(a.Kind))
	return b.String()
}

// DispatchPrompt builds the focused per-anomaly Claude prompt the agent
// dispatcher hands to claude -p. Each kind gets a short, surgical script.
// issueRepo is the "owner/name" repository where GitHub issues must be filed;
// it is injected explicitly so agents are independent of their working
// directory (which may be a task worktree for an unrelated project).
func DispatchPrompt(a Anomaly, issueRepo string) string {
	switch a.Kind {
	case KindPRGap:
		return prGapPrompt(a)
	case KindStuckHumanBlocked:
		return stuckPrompt(a, issueRepo)
	case KindFailureSpike:
		return failureSpikePrompt(a, issueRepo)
	case KindBottleneck:
		return bottleneckPrompt(a, issueRepo)
	default:
		return investigatePrompt(a, issueRepo)
	}
}

func prGapPrompt(a Anomaly) string {
	taskID, _ := a.Evidence["task_id"].(string)
	title, _ := a.Evidence["title"].(string)
	return fmt.Sprintf(`You are the sybra monitor PR-gap remediator.

Task: %s — %q
This task is in 'in-review' but has no PR number recorded.
Your working directory is the task's worktree.

Run, in order:

1. `+"`git status`"+` and `+"`git log --oneline -5 origin/main..HEAD`"+` to confirm there are commits ahead of origin/main.
2. If there are no commits ahead:
   `+"`sybra-cli update %s --status human-required --status-reason \"monitor: in-review with no commits\"`"+`
   then exit.
3. Otherwise:
   `+"`git push -u origin HEAD`"+`
   `+"`gh pr create --base main --title %q --body \"<two-sentence summary from the latest commits>\"`"+`
4. On success, run `+"`sybra-cli update %s --pr <number> --status-reason \"monitor: created missing PR\"`"+`.

Output exactly one final JSON line:
{"action":"created"|"escalated"|"failed","prNumber":N,"reason":"..."}`,
		taskID, title, taskID, title, taskID,
	)
}

func stuckPrompt(a Anomaly, issueRepo string) string {
	taskID, _ := a.Evidence["task_id"].(string)
	title, _ := a.Evidence["title"].(string)
	status, _ := a.Evidence["status"].(string)
	dwell, _ := a.Evidence["dwell_h"].(float64)
	filePath, _ := a.Evidence["file_path"].(string)
	return fmt.Sprintf(`You are the sybra monitor stuck-task investigator.

Task: %s — %q
Status: %s   Dwell: %.1fh
Task file: %s

Read-only investigation:
- Read the task file and the most recent agent log under ~/.sybra/logs/agents matching this task id.
- Identify the actual blocker in one sentence and propose the next concrete step a human could take.
- Then dedup against open issues and either create or comment one on the repo below.

GitHub issue handling:
- Repo: %s
- Title: "[monitor] stuck_human_blocked: %s"
- Dedup: gh issue list --repo %s --state open --label monitor --search "in:title \"[monitor] stuck_human_blocked: %s\""
- On hit: gh issue comment --repo %s <num> --body "..."
- On miss: gh issue create --repo %s --title "[monitor] stuck_human_blocked: %s" --body "..." --label monitor,bug

Output exactly one final JSON line:
{"issueNumber":N,"action":"created"|"commented","blocker":"<one phrase>","nextStep":"<imperative sentence>"}`,
		taskID, title, status, dwell, filePath,
		issueRepo, taskID, issueRepo, taskID, issueRepo, issueRepo, taskID,
	)
}

func failureSpikePrompt(a Anomaly, issueRepo string) string {
	rate, _ := a.Evidence["failure_rate"].(float64)
	runs, _ := a.Evidence["agent_runs"].(int)
	return fmt.Sprintf(`You are the sybra monitor failure-spike investigator.

Audit summary (last 1h):
  failure_rate=%.2f  agent_runs=%d

Read-only investigation:
- Run `+"`sybra-cli --json audit --since 1h --type agent.failed`"+` to list failed agents.
- For up to 3 most recent failures, read the agent NDJSON log under ~/.sybra/logs/agents and identify the proximate cause.
- Look for a common pattern across failures (provider error, tool error, repeated tool loop, etc).

GitHub issue handling:
- Repo: %s
- Title: "[monitor] failure_spike"
- Dedup: gh issue list --repo %s --state open --label monitor --search "in:title \"[monitor] failure_spike\""
- On hit: gh issue comment --repo %s <num> --body "..."
- On miss: gh issue create --repo %s --title "[monitor] failure_spike" --body "..." --label monitor,bug

Output exactly one final JSON line:
{"issueNumber":N,"action":"created"|"commented","rootCause":"<one phrase>","commonPattern":"<one phrase>"}`,
		rate, runs, issueRepo, issueRepo, issueRepo, issueRepo,
	)
}

func bottleneckPrompt(a Anomaly, issueRepo string) string {
	status, _ := a.Evidence["status"].(string)
	dwell, _ := a.Evidence["dwell_h"].(float64)
	threshold, _ := a.Evidence["threshold"].(float64)
	return fmt.Sprintf(`You are the sybra monitor bottleneck investigator.

Status %q has average dwell %.1fh, exceeding threshold %.1fh.

Read-only investigation:
- Run `+"`sybra-cli --json list --status %s`"+` and read the 3 oldest task bodies under ~/.sybra/tasks.
- Identify whether the bottleneck is structural (workflow rule), human (waiting on a person), or process (slow handoff between statuses).

GitHub issue handling:
- Repo: %s
- Title: "[monitor] bottleneck: %s"
- Dedup: gh issue list --repo %s --state open --label monitor --search "in:title \"[monitor] bottleneck: %s\""
- On hit: gh issue comment --repo %s <num> --body "..."
- On miss: gh issue create --repo %s --title "[monitor] bottleneck: %s" --body "..." --label monitor,bug

Output exactly one final JSON line:
{"issueNumber":N,"action":"created"|"commented","likelyCause":"<phrase>","affectedTaskIds":[...]}`,
		status, dwell, threshold, status, issueRepo, status, issueRepo, status, issueRepo, issueRepo, status,
	)
}

// investigatePrompt is the catch-all handler for kinds that have no specific
// template — should never run today, but keeps DispatchPrompt total.
func investigatePrompt(a Anomaly, issueRepo string) string {
	return fmt.Sprintf(`You are the sybra monitor anomaly investigator.

Anomaly: %s
Fingerprint: %s
Evidence:
%s

Read the relevant logs under ~/.sybra/logs/, identify the proximate cause,
and either create or comment an issue at %s with label "monitor".
Always pass --repo %s to gh commands.

Output one final JSON line: {"issueNumber":N,"action":"created"|"commented","summary":"..."}`,
		a.Kind, a.Fingerprint, evidenceJSON(a.Evidence), issueRepo, issueRepo,
	)
}

func evidenceJSON(ev map[string]any) string {
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func suggestedInvestigation(kind AnomalyKind) string {
	switch kind {
	case KindOverDispatchLimit:
		return "- Cap concurrent agents in `agent.MaxConcurrent` or stop in-progress runs that have been live longer than expected.\n"
	case KindLostAgent:
		return "- Confirm the agent process actually exited; the watchdog has reset the task to `todo`.\n"
	case KindUntriaged:
		return "- Run `/sybra-triage` against the affected task to fill `agent_mode` and `tags`.\n"
	default:
		return "- See the dispatched agent's issue comment for proximate cause and next step.\n"
	}
}

// SortAnomalies returns the same slice with a stable ordering so reports are
// deterministic across ticks (useful for snapshot tests and human reviewers).
func SortAnomalies(anoms []Anomaly) []Anomaly {
	slices.SortStableFunc(anoms, func(a, b Anomaly) int {
		if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return cmp.Compare(a.Fingerprint, b.Fingerprint)
	})
	return anoms
}
