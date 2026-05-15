package monitor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/task"
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
	fmt.Fprintf(&b, "## Suggested investigation\n%s\n", suggestedInvestigation(a))
	return b.String()
}

// DispatchPrompt builds the focused per-anomaly Claude prompt the agent
// dispatcher hands to claude -p. Each kind gets a short, surgical script.
// issueRepo is the "owner/name" repository where GitHub issues must be filed;
// it is injected explicitly so agents are independent of their working
// directory (which may be a task worktree for an unrelated project).
// pushRemote names the git remote agents should push branches to ("fork"
// when the worktree's repo has a user-fork remote configured, else "origin").
func DispatchPrompt(a Anomaly, issueRepo, pushRemote string) string {
	switch a.Kind {
	case KindPRGap:
		return prGapPrompt(a, pushRemote)
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

func prGapPrompt(a Anomaly, pushRemote string) string {
	taskID, _ := a.Evidence["task_id"].(string)
	title, _ := a.Evidence["title"].(string)
	if pushRemote == "" {
		pushRemote = "origin"
	}
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
   `+"`git push -u %s HEAD`"+`
   `+"`gh pr create --base main --title %q --body \"<two-sentence summary from the latest commits>\"`"+`
   When pushing to a fork remote, gh detects the cross-repo head automatically; no extra flags needed.
4. On success, run `+"`sybra-cli update %s --pr <number> --status-reason \"monitor: created missing PR\"`"+`.

Output exactly one final JSON line:
{"action":"created"|"escalated"|"failed","prNumber":N,"reason":"..."}`,
		taskID, title, taskID, pushRemote, title, taskID,
	)
}

func stuckPrompt(a Anomaly, issueRepo string) string {
	taskID, _ := a.Evidence["task_id"].(string)
	title, _ := a.Evidence["title"].(string)
	status, _ := a.Evidence["status"].(string)
	dwell, _ := a.Evidence["dwell_h"].(float64)
	filePath, _ := a.Evidence["file_path"].(string)
	statusReason, _ := a.Evidence["status_reason"].(string)
	lastRole, _ := a.Evidence["last_agent_role"].(string)
	lastState, _ := a.Evidence["last_agent_state"].(string)
	prNumber, _ := a.Evidence["pr_number"].(int)

	var extra strings.Builder
	if statusReason != "" {
		fmt.Fprintf(&extra, "Status reason: %s\n", statusReason)
	}
	if lastRole != "" {
		fmt.Fprintf(&extra, "Last agent: role=%s state=%s\n", lastRole, lastState)
	} else if lastState != "" {
		fmt.Fprintf(&extra, "Last agent: state=%s\n", lastState)
	}
	if prNumber > 0 {
		fmt.Fprintf(&extra, "Linked PR: #%d\n", prNumber)
	}
	extraStr := extra.String()

	investigationHint := `- Read the task file and the most recent agent log under ~/.sybra/logs/agents matching this task id.
- Identify the actual blocker in one sentence and propose the next concrete step a human could take.`
	if lastRole == "human-review" && lastState == "stopped" {
		investigationHint = `- A human-review agent has already assessed this task. Read the "## Auto-review verdict" section (or the most recent "## Re-detected" block) in the task file — use that verdict as the blocker summary instead of re-reading the agent log.
- If the verdict says "awaiting PR reviewer": confirm the PR is still open and unreviewed, then report that as the blocker with "request review / wait for reviewer" as the next step.
- If the verdict note contains "sybra_bug (no issue payload)", "sybra_bug (local task creation failed)", or "sybra_bug (issue submission failed)": issue filing failed on a transient or empty-payload error — the task is correctly in human-required. Report the specific failure text as the blocker with "retry issue filing or investigate the filing error" as the next step.
- If the verdict note says "sybra_bug" without a failure qualifier in parentheses: the task should be in blocked status, not human-required; report that as the blocker.`
	} else if lastRole == "fix-review" && lastState == "stopped" {
		prRef := "the PR"
		if prNumber > 0 {
			prRef = fmt.Sprintf("PR #%d", prNumber)
		}
		doneCmd := ""
		if taskID != "" {
			doneCmd = "\n- Once " + prRef + " merges, mark task done: `sybra-cli update " + taskID + " --status done`."
		}
		mergedCmd := ""
		if taskID != "" {
			mergedCmd = "`sybra-cli update " + taskID + " --status done`"
		}
		investigationHint = "- A fix-review agent already ran — skip the agent log and check " + prRef + " state with `gh pr view --json state,reviewDecision,statusCheckRollup`.\n" +
			"- If state=MERGED: the PR has already been merged. Run " + mergedCmd + ". Skip issue filing and output: {\"issueNumber\":null,\"action\":\"remediated\",\"blocker\":\"PR already merged\",\"nextStep\":\"none\"}.\n" +
			"- If CHANGES_REQUESTED: new review comments arrived — report that as the blocker with \"run another fix-review agent\" as the next step.\n" +
			"- If REVIEW_REQUIRED: fixes were pushed but review was not re-requested — report \"awaiting re-review\" with \"re-request review\" as the next step.\n" +
			"- If APPROVED and CI passes: report \"ready to merge\" with \"merge the PR\" as the next step." +
			doneCmd
	}

	return fmt.Sprintf(`You are the sybra monitor stuck-task investigator.

Task: %s — %q
Status: %s   Dwell: %.1fh
Task file: %s
%s
Read-only investigation:
%s
- Then dedup against open issues and either create or comment one on the repo below.

GitHub issue handling:
- Repo: %s
- Title: "[monitor] stuck_human_blocked: %s"
- Dedup: gh issue list --repo %s --state open --label monitor --search "in:title \"[monitor] stuck_human_blocked: %s\""
- On hit: gh issue comment --repo %s <num> --body "..."
- On miss: gh issue create --repo %s --title "[monitor] stuck_human_blocked: %s" --body "..." --label monitor,bug

Output exactly one final JSON line:
{"issueNumber":N,"action":"created"|"commented"|"remediated","blocker":"<one phrase>","nextStep":"<imperative sentence>"}`,
		taskID, title, status, dwell, filePath, extraStr, investigationHint,
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

func suggestedInvestigation(a Anomaly) string {
	switch a.Kind {
	case KindOverDispatchLimit:
		return "- Cap concurrent agents in `agent.MaxConcurrent` or stop in-progress runs that have been live longer than expected.\n"
	case KindLostAgent:
		return "- Confirm the agent process actually exited; the watchdog has reset the task to `todo`.\n"
	case KindUntriaged:
		return "- Run `/sybra-triage` against the affected task to fill `agent_mode` and `tags`.\n"
	case KindStuckHumanBlocked:
		if status, _ := a.Evidence["status"].(string); status == string(task.StatusPlanReview) {
			return "- Approve or reject the plan to advance the task.\n"
		}
		hint := "- Review the task file and `status_reason`, then provide the required human input to unblock progress.\n"
		if reason, _ := a.Evidence["status_reason"].(string); reason != "" {
			hint += "- Blocking reason: " + reason + "\n"
		}
		taskID, _ := a.Evidence["task_id"].(string)
		prNum, _ := a.Evidence["pr_number"].(int)
		lastRole, _ := a.Evidence["last_agent_role"].(string)
		lastState, _ := a.Evidence["last_agent_state"].(string)
		if lastRole == "human-review" && lastState == "stopped" {
			verdict, _ := a.Evidence["human_review_verdict"].(string)
			if verdict == "human" {
				hint += "- Human-review agent confirmed: this task requires direct human input (scope beyond automation).\n"
				if taskID != "" {
					if prNum > 0 {
						hint += fmt.Sprintf("- If waiting on PR review: check PR #%d for new reviewer feedback.\n", prNum)
					}
					hint += "- To hand off for interactive work: `sybra-cli update " + taskID + " --mode interactive --status todo`.\n"
				}
			} else {
				hint += "- Human-review agent assessed this task — check the latest auto-review note in the task body.\n"
				if taskID != "" {
					if prNum > 0 {
						hint += fmt.Sprintf("- If note says awaiting PR review: check PR #%d for new reviewer feedback.\n", prNum)
					}
					hint += "- Note confirms human input needed: provide it, then `sybra-cli update " + taskID + " --status todo`.\n"
					hint += "- If the note says scope exceeds automation, switch to interactive mode: `sybra-cli update " + taskID + " --mode interactive --status todo`.\n"
				}
				hint += "- Note shows unparseable or failed verdict: review the raw agent output in the note and act accordingly.\n"
			}
		} else if lastRole == "fix-review" && lastState == "stopped" {
			hint += "- Fix-review agent finished — check the PR and agent log for the outcome.\n"
			if prNum > 0 {
				hint += fmt.Sprintf("- Check PR #%d state: if MERGED mark task done immediately (`sybra-cli update %s --status done`); if CHANGES_REQUESTED run another fix-review agent; if REVIEW_REQUIRED re-request review; if APPROVED and CI passes merge.\n", prNum, taskID)
			} else {
				hint += "- Check the PR state: if already merged mark task done; address remaining comments, re-request review, or merge if approved.\n"
			}
			if taskID != "" {
				hint += "- Once PR merges, mark task done: `sybra-cli update " + taskID + " --status done`.\n"
			}
		}
		return hint
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
