package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var prURLRe = regexp.MustCompile(`github\.com/[^/\s]+/[^/\s]+/pull/(\d+)`)
var prShortRe = regexp.MustCompile(`\b[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#(\d+)`)

const (
	workflowRetryAfterVar     = "workflow.retry_after"
	prCreateRetryBackoff      = 15 * time.Minute
	prCreateRetryStatusReason = "GitHub rate limit during PR creation — retrying later"
)

// execLinkPRAndReview is a non-LLM mechanical step that tries to recover the
// PR number from three sources and flip the task to in-review:
//
//  1. task.pr_number already set → set in-review, skip eval
//  2. regex match on agent result text in step history → link + in-review
//  3. gh pr list --head <branch> → single result → link + in-review
//
// When no PR is found the step returns without touching task status, allowing
// the workflow to fall through to the LLM eval step.
func (e *Engine) execLinkPRAndReview(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	setInReview := func(prNumber int, source string) (StepOutput, error) {
		if err := e.tasks.UpdateTaskPR(taskID, prNumber); err != nil {
			return StepOutput{}, fmt.Errorf("link pr: %w", err)
		}
		if err := e.tasks.UpdateTaskStatus(taskID, "in-review", ""); err != nil {
			return StepOutput{}, fmt.Errorf("set in-review: %w", err)
		}
		msg := fmt.Sprintf("pr #%d found via %s → in-review", prNumber, source)
		e.logger.Info("workflow.link-pr.linked", "task_id", taskID, "pr", prNumber, "source", source)
		return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
	}

	// Path 1: PR already linked on task.
	if t.PRNumber > 0 {
		return setInReview(t.PRNumber, "task.pr_number")
	}

	// Path 2: Scan step history for a GitHub PR URL or owner/repo#N in agent output.
	for i := range slices.Backward(wfExec.StepHistory) {
		rec := wfExec.StepHistory[i]
		if rec.Status != "completed" || rec.Output == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{prURLRe, prShortRe} {
			if m := re.FindStringSubmatch(rec.Output); len(m) > 1 {
				n, err := strconv.Atoi(m[1])
				if err == nil && n > 0 {
					return setInReview(n, "agent result")
				}
			}
		}
	}

	// Path 3: Query GitHub when branch is known.
	// Use bash -c with env vars to keep project/branch out of arg list (gosec G204).
	if t.ProjectID != "" && t.Branch != "" {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c",
			"gh pr list --repo \"$_REPO\" --head \"$_BRANCH\" --json number --limit 2")
		cmd.Env = append(cmd.Environ(), "_REPO="+t.ProjectID, "_BRANCH="+t.Branch)
		out, err := cmd.Output()
		if err != nil {
			e.logger.Warn("workflow.link-pr.gh-list", "task_id", taskID, "err", err)
		} else {
			var prs []struct {
				Number int `json:"number"`
			}
			jsonErr := json.Unmarshal(out, &prs)
			if jsonErr == nil && len(prs) == 1 {
				return setInReview(prs[0].Number, "gh pr list")
			}
			if jsonErr != nil {
				// gh --json returned malformed output. Don't mask the upstream
				// failure as "no pr found" — log so operators can diagnose.
				e.logger.Warn("workflow.link-pr.gh-list.parse", "task_id", taskID, "err", jsonErr, "raw", truncate(string(out), 200))
			}
		}
	}

	e.logger.Info("workflow.link-pr.no-pr", "task_id", taskID)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "no pr found: falling through to eval"}, nil
}

// execEvaluate is a non-LLM mechanical step that decides the terminal status
// after link_pr_and_review has exhausted its PR-discovery paths. It walks step
// history backwards for the most recent run_agent record (the impl/fix step)
// and flips the task to human-required with a bounded reason string.
//
// Before giving up, it does a final gh pr list check — guarding against the
// race where the agent created a PR after link_pr_and_review already ran.
func (e *Engine) execEvaluate(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Final GitHub PR check — catches PRs created after link_pr_and_review ran.
	if t.ProjectID != "" && t.Branch != "" {
		ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c",
			"gh pr list --repo \"$_REPO\" --head \"$_BRANCH\" --json number --limit 2")
		cmd.Env = append(cmd.Environ(), "_REPO="+t.ProjectID, "_BRANCH="+t.Branch)
		if out, err := cmd.Output(); err == nil {
			var prs []struct {
				Number int `json:"number"`
			}
			jsonErr := json.Unmarshal(out, &prs)
			if jsonErr == nil && len(prs) == 1 {
				prNum := prs[0].Number
				if linkErr := e.tasks.UpdateTaskPR(taskID, prNum); linkErr != nil {
					return StepOutput{}, fmt.Errorf("evaluate: link pr: %w", linkErr)
				}
				if linkErr := e.tasks.UpdateTaskStatus(taskID, "in-review", ""); linkErr != nil {
					return StepOutput{}, fmt.Errorf("evaluate: set in-review: %w", linkErr)
				}
				msg := fmt.Sprintf("pr #%d found via late gh pr list → in-review", prNum)
				e.logger.Info("workflow.evaluate.late-pr-found", "task_id", taskID, "pr", prNum)
				return StepOutput{StepID: step.ID, Status: "completed", Output: msg}, nil
			}
			if jsonErr != nil {
				e.logger.Warn("workflow.evaluate.gh-list.parse", "task_id", taskID, "err", jsonErr, "raw", truncate(string(out), 200))
			}
		}
	}

	var last *StepRecord
	for i := range slices.Backward(wfExec.StepHistory) {
		if wfExec.StepHistory[i].AgentID != "" {
			last = &wfExec.StepHistory[i]
			break
		}
	}

	reason := "no agent result to evaluate"
	if last != nil {
		if isPRCreationStep(last.StepID) && looksLikeGitHubRateLimit(last.Output) {
			wfExec.CurrentStep = last.StepID
			wfExec.State = ExecWaiting
			wfExec.SetVar(workflowRetryAfterVar, time.Now().UTC().Add(prCreateRetryBackoff).Format(time.RFC3339))
			if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
				return StepOutput{}, err
			}
			if statusErr := e.tasks.UpdateTaskStatus(taskID, t.Status, prCreateRetryStatusReason); statusErr != nil {
				return StepOutput{}, statusErr
			}
			e.logger.Warn("workflow.evaluate.pr-create-rate-limited", "task_id", taskID, "step", last.StepID)
			return StepOutput{}, errStepParked
		}
		if last.Status == "failed" {
			reason = truncate(strings.TrimSpace(last.Output), 200)
			if reason == "" {
				reason = "agent failed with no output"
			}
		} else {
			reason = "commits pushed but no PR created"
		}
	}

	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("evaluate: set human-required: %w", err)
	}
	e.logger.Info("workflow.evaluate.human-required", "task_id", taskID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

func isPRCreationStep(stepID string) bool {
	return stepID == "create_pr" || stepID == "push_existing_pr"
}

func looksLikeGitHubRateLimit(output string) bool {
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "rate limit") {
		return false
	}
	return strings.Contains(lower, "github") ||
		strings.Contains(lower, "graphql") ||
		strings.Contains(lower, "gh ") ||
		strings.Contains(lower, "api rate limit") ||
		strings.Contains(lower, "secondary rate limit")
}
