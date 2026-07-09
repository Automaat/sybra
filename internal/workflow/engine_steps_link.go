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
	workflowRetryAfterVar         = "workflow.retry_after"
	prCreateRetryBackoff          = 15 * time.Minute
	prCreateRetryStatusReason     = "GitHub rate limit during PR creation — retrying later"
	prCreateTransientStatusReason = "GitHub connectivity issue during PR creation — retrying later"
	prCreateAuthRetryReason       = "GitHub authentication issue during PR creation — retrying later"
	prCreatePushedNoPRReason      = "commits pushed but no PR created — retrying"
	prCreateAttemptsVar           = "workflow.pr_create_attempts"
	maxPRCreatePushedNoPRRetries  = 3
	prCreateAuthAttemptsVar       = "workflow.pr_create_auth_attempts"
	maxPRCreateAuthRetries        = 3
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
		if isPRCreationStep(last.StepID) {
			switch {
			case looksLikeGitHubRateLimit(last.Output):
				return e.parkStepForRetry(taskID, wfExec, t, last.StepID, prCreateRetryStatusReason, "workflow.evaluate.pr-create-rate-limit")
			case looksLikeTransientGitHub(last.Output):
				return e.parkStepForRetry(taskID, wfExec, t, last.StepID, prCreateTransientStatusReason, "workflow.evaluate.pr-create-transient-outage")
			case looksLikeAuthFailure(last.Output):
				// Bad/expired credentials are not naturally time-bounded like a
				// rate limit or a network blip, so they only get a bounded
				// number of retries before escalating to a human.
				attempts := parseWorkflowInt(wfExec.Variables[prCreateAuthAttemptsVar])
				if attempts < maxPRCreateAuthRetries {
					wfExec.SetVar(prCreateAuthAttemptsVar, strconv.Itoa(attempts+1))
					return e.parkStepForRetry(taskID, wfExec, t, last.StepID, prCreateAuthRetryReason, "workflow.evaluate.pr-create-auth-retry", "attempt", attempts+1, "max", maxPRCreateAuthRetries)
				}
				reason = fmt.Sprintf("PR creation failing due to invalid or expired GitHub credentials after %d retries", attempts)
			}
		}
		if reason == "no agent result to evaluate" {
			switch {
			case last.Status == "failed":
				reason = truncate(strings.TrimSpace(last.Output), 200)
				if reason == "" {
					reason = "agent failed with no output"
				}
			case isPRCreationStep(last.StepID):
				// Agent "succeeded" (didn't self-report failure) but no PR was found
				// by link_pr_and_review or the late gh pr list check above. The
				// worktree/branch still exist and re-checking for a race-created PR
				// is safe, so retry a bounded number of times before giving up.
				attempts := parseWorkflowInt(wfExec.Variables[prCreateAttemptsVar])
				if attempts < maxPRCreatePushedNoPRRetries {
					wfExec.SetVar(prCreateAttemptsVar, strconv.Itoa(attempts+1))
					return e.parkStepForRetry(taskID, wfExec, t, last.StepID, prCreatePushedNoPRReason, "workflow.evaluate.pr-create-pushed-no-pr-retry", "attempt", attempts+1, "max", maxPRCreatePushedNoPRRetries)
				}
				reason = fmt.Sprintf("commits pushed but no PR created after %d retries", attempts)
			default:
				reason = "commits pushed but no PR created"
			}
		}
	}

	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, fmt.Errorf("evaluate: set human-required: %w", err)
	}
	e.logger.Info("workflow.evaluate.human-required", "task_id", taskID, "reason", reason)
	return StepOutput{StepID: step.ID, Status: "completed", Output: reason}, nil
}

// parkStepForRetry rewinds wfExec back to stepID and persists a bounded
// retry-after window, then parks the current evaluate step via errStepParked.
func (e *Engine) parkStepForRetry(taskID string, wfExec *Execution, t TaskInfo, stepID, statusReason, logEvent string, logAttrs ...any) (StepOutput, error) {
	wfExec.CurrentStep = stepID
	wfExec.State = ExecWaiting
	wfExec.SetVar(workflowRetryAfterVar, time.Now().UTC().Add(prCreateRetryBackoff).Format(time.RFC3339))
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return StepOutput{}, err
	}
	if statusErr := e.tasks.UpdateTaskStatus(taskID, t.Status, statusReason); statusErr != nil {
		return StepOutput{}, statusErr
	}
	attrs := append([]any{"task_id", taskID, "step", stepID, "reason", statusReason}, logAttrs...)
	e.logger.Warn(logEvent, attrs...)
	return StepOutput{}, errStepParked
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

// looksLikeTransientGitHub matches network-level failures that keep a PR
// creation agent from reaching GitHub at all — DNS/connection errors, TLS
// failures, timeouts, and 502/503 responses. These are naturally time-bounded
// (retrying once connectivity is restored is safe) and are distinct from
// looksLikeGitHubRateLimit (requires "rate limit") and looksLikeAuthFailure
// (credential problems, which are not naturally time-bounded).
func looksLikeTransientGitHub(output string) bool {
	lower := strings.ToLower(output)
	patterns := []string{
		"connection refused",
		"could not resolve host",
		"no such host",
		"temporary failure in name resolution",
		"i/o timeout",
		"timed out",
		"context deadline exceeded",
		"tls handshake",
		"tls:",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return looksLikeGatewayStatus(lower)
}

// looksLikeGatewayStatus matches HTTP 502/503 responses regardless of how the
// status is phrased ("HTTP 502", "502 bad gateway", "503 Service Unavailable",
// ...) — GitHub/gh can surface either the bare code or a reason phrase.
func looksLikeGatewayStatus(lower string) bool {
	for _, code := range []string{"502", "503"} {
		idx := strings.Index(lower, code)
		if idx < 0 {
			continue
		}
		// Guard against matching a status embedded in a larger token
		// (e.g. "15029" or "abc502def") by requiring non-alphanumeric
		// boundaries around the code.
		if idx > 0 && !isStatusBoundary(lower[idx-1]) {
			continue
		}
		end := idx + len(code)
		if end < len(lower) && !isStatusBoundary(lower[end]) {
			continue
		}
		return true
	}
	return false
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isLowerAlpha(b byte) bool {
	return b >= 'a' && b <= 'z'
}

func isStatusBoundary(b byte) bool {
	return !isDigit(b) && !isLowerAlpha(b)
}

// looksLikeAuthFailure matches bad/expired GitHub credentials. Unlike rate
// limits or network blips, a broken token does not self-heal, so callers must
// bound how many times they retry before escalating to a human.
func looksLikeAuthFailure(output string) bool {
	lower := strings.ToLower(output)
	patterns := []string{
		"bad credentials",
		"authentication failed",
		"gh auth",
		"401 unauthorized",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}
