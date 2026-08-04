package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/metrics"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// branchConflictFixWorkflowID is the builtin workflow started directly (by
// ID, via StartWorkflowWithVars) to recover a task-branch conflict without
// going through the ordinary PR-fix dispatch path. It is never reached via
// DispatchEvent/trigger matching — see
// internal/workflow/builtin/branch-conflict-fix.yaml.
const branchConflictFixWorkflowID = "branch-conflict-fix"

// prFixWorkflowID is the workflow handlePRIssueReplacingWorkflow dispatches
// for a PR-numbered conflict recovery (RecoverStaleBranchConflict's
// t.PRNumber != 0 branch). Used the same way as branchConflictFixWorkflowID:
// to detect a recovery that already dispatched and is merely waiting on a
// pool/dispatch slot, so a re-entrant caller doesn't cancel and restart it.
const prFixWorkflowID = "pr-fix"

const branchConflictRetryKind = github.PRIssueBranchConflictNoPR
const sameBranchConflictRetryKind = github.PRIssueTaskBranchConflict
const branchRecreateKind = github.PRIssueBranchRecreate
const ciInfraRerunKind = github.PRIssueKind("ci_infra_rerun")

const wtFailureLimit = 5

// branchConflictDispatchFailureLimit bounds retries for a transient
// provider-unhealthy/rate-limit error starting the branch-conflict-fix
// workflow itself (dispatchBranchConflictRecovery). Distinct from
// wtFailureLimit (worktree-prep failures) and prTracker's
// branchConflictRetryKind budget (workflow attempts that actually started):
// this only guards the dispatch call itself, so a transient provider outage
// doesn't fall straight through to a human-required escalation even though
// the branch conflict itself was never actually attempted.
const branchConflictDispatchFailureLimit = 5

type branchConflictResumeState struct {
	status       string
	statusReason string
	workflowID   string
	workflowStep string
	workflowVars string
	prior        *workflow.Execution
}

type taskBranchConflictRecoverySpec struct {
	retryKind      github.PRIssueKind
	branchOverride string
	remoteOverride string
	trustedOrigin  bool
	allowRecreate  bool
	prompt         func(context.Context, task.Task, string) string
}

type dispatchFixOptions struct {
	replaceActiveWorkflow bool
	cancelReason          string
}

const PRFixResultContract = "\n\nBefore your final response, decide the outcome:\n" +
	"- If you completed the fix, end with `SYBRA_PR_FIX_RESULT: continue`.\n" +
	"- If this PR's diff is correct and CI failed for reasons unrelated to it — a " +
	"flaky test, an infrastructure failure, or a breakage that reproduces on the " +
	"base branch — end with `SYBRA_PR_FIX_RESULT: flake` and `SYBRA_PR_FIX_REASON: " +
	"<what failed, and the evidence it is unrelated>`. Reporting `flake` with " +
	"evidence is a complete and successful outcome. It is always the right answer " +
	"over inventing a change you cannot causally justify.\n" +
	"- If you intentionally stopped because the PR needs a human, end with " +
	"`SYBRA_PR_FIX_RESULT: human-required` and `SYBRA_PR_FIX_REASON: <short reason>`. " +
	"If the reason is specific test failures you already found while " +
	"investigating (not e.g. a missing credential or an ambiguous scope " +
	"decision), also emit one `SYBRA_PR_FIX_FAILING_TEST: <package/file:line " +
	"test-name>` line per failing test, so the next agent to pick this up " +
	"gets exact repro info instead of having to rediscover it."

// MergePolicy selects which review signal MergeGate.ReadyForMerge accepts as
// having satisfied the PR's review cycle.
type MergePolicy int

const (
	// MergePolicyCopilot requires GitHub Copilot to have reviewed the PR. It
	// also doubles as the REST-sourced default policy: REST never populates
	// CopilotReviewed/UnresolvedCount/ReviewDecision, so every REST-sourced
	// merge (other than Renovate's bypass) is gated on RESTApproved instead,
	// regardless of which of these four values is passed.
	MergePolicyCopilot MergePolicy = iota
	// MergePolicySybraReviewed requires the owning task to have completed
	// Sybra's own review stage. GraphQL-sourced only.
	MergePolicySybraReviewed
	// MergePolicyOwnBot requires the PR to be authored by Sybra's own bot
	// identity. GraphQL-sourced only.
	MergePolicyOwnBot
	// MergePolicyRenovate waives the review-signal requirement entirely
	// (Renovate PRs are never reviewed) and merges on green CI + mergeable +
	// !draft alone.
	MergePolicyRenovate
)

// MergeGate is the single owner of "is this PR ready to merge" and "is this
// PR ready to have native auto-merge armed" — the six near-identical
// predicates this consolidates (readyForCopilotAutoMerge,
// readyForSybraReviewedAutoMerge, readyForOwnBotAutoMerge,
// readyForRESTAutoMerge, restRenovateGreen, readyToArmNativeAutoMerge) each
// differed only in which source populated the PR (GraphQL vs REST) and which
// review signal the policy accepts.
type MergeGate struct {
	pr github.PullRequest
}

// NewMergeGate builds a MergeGate over a single PR snapshot.
func NewMergeGate(pr github.PullRequest) MergeGate {
	return MergeGate{pr: pr}
}

// ciGreen reports CI green-or-absent, shared by every policy.
func (g MergeGate) ciGreen() bool {
	return g.pr.CIStatus == "SUCCESS" || g.pr.CIStatus == ""
}

// baseReady is the source-appropriate mechanical-mergeability gate: not
// draft, and mergeable per whichever fetch populated the PR. A REST-sourced
// PR requires GitHub's raw mergeable_state to be exactly "clean" (blocked/
// behind/unstable/unknown do NOT authorize it) and both REST CI legs to have
// been fetched (an unfetched CI status must never read as green).
func (g MergeGate) baseReady() bool {
	if g.pr.IsDraft {
		return false
	}
	if g.pr.SourcedViaREST {
		return g.pr.RESTMergeableState == "clean" && g.pr.RESTCIFetched
	}
	return g.pr.Mergeable == "MERGEABLE"
}

// threadsClear reports no unresolved review threads and no outstanding
// change request — the GraphQL-only review-cycle signal REST never
// populates.
func (g MergeGate) threadsClear() bool {
	return g.pr.UnresolvedCount == 0 && g.pr.ReviewDecision != "CHANGES_REQUESTED"
}

// ReadyForMerge reports whether the PR satisfies policy's merge gate.
// sybraReviewed carries the owning task's Reviewed field; it is only
// consulted for MergePolicySybraReviewed.
func (g MergeGate) ReadyForMerge(policy MergePolicy, sybraReviewed bool) bool {
	if !g.baseReady() || !g.ciGreen() {
		return false
	}
	if g.pr.SourcedViaREST {
		if policy == MergePolicyRenovate {
			return true
		}
		return g.pr.RESTApproved
	}
	switch policy {
	case MergePolicyCopilot:
		return g.pr.CopilotReviewed && g.threadsClear()
	case MergePolicySybraReviewed:
		return sybraReviewed && g.threadsClear()
	case MergePolicyOwnBot:
		return g.pr.SelfAuthoredBot && g.threadsClear()
	case MergePolicyRenovate:
		return true
	default:
		return false
	}
}

// BlockedOnlyByThreads reports whether the PR meets every Copilot auto-merge
// condition except the unresolved-threads check — the precise state in which
// resolving addressed Copilot threads can unblock a merge.
func (g MergeGate) BlockedOnlyByThreads() bool {
	if g.pr.IsDraft || g.pr.Mergeable != "MERGEABLE" || !g.ciGreen() {
		return false
	}
	return g.pr.CopilotReviewed && g.pr.ReviewDecision != "CHANGES_REQUESTED" && g.pr.UnresolvedCount > 0
}

// ReadyToArm reports whether a PR is ready to have GitHub's native
// auto-merge armed: the same review-cycle gate as
// ReadyForMerge(MergePolicyCopilot) MINUS the CI-green requirement (native
// auto-merge itself waits for CI to go green) PLUS excluding a PR whose CI is
// already FAILURE — native auto-merge won't retry a hard failure, so arming
// on red CI would just strand it — and PRs already armed or bot-authored by
// Renovate (its own bypass path already merges without this gate).
func (g MergeGate) ReadyToArm() bool {
	if g.pr.IsDraft || g.pr.Mergeable != "MERGEABLE" {
		return false
	}
	if g.pr.CIStatus == "FAILURE" || g.pr.AutoMergeEnabled || g.pr.Author == "renovate[bot]" {
		return false
	}
	return g.pr.CopilotReviewed && g.threadsClear()
}

// StateSignature fingerprints the PR fields every merge/arm policy reads, so
// callers can detect "nothing relevant changed" between polls.
func (g MergeGate) StateSignature() string {
	pr := g.pr
	return fmt.Sprintf("%s|%t|%s|%s|%s|%d|%t|%t|%s|%t|%t",
		pr.UpdatedAt,
		pr.IsDraft,
		pr.CIStatus,
		pr.Mergeable,
		pr.ReviewDecision,
		pr.UnresolvedCount,
		pr.CopilotReviewed,
		pr.AutoMergeEnabled,
		pr.RESTMergeableState,
		pr.RESTCIFetched,
		pr.RESTApproved,
	)
}

type nativeAutoMergeAttemptResult struct {
	armed     bool
	attempted bool
	err       error
}

// preflightArmNativeAutoMerge prefers arming GitHub's native auto-merge over
// Sybra's own squash merge when it's available and the PR is otherwise ready
// — it's cheaper (REST poll on GitHub's side) than Sybra's GraphQL merge-gate
// polling. Only tried once the CI-green-gated legacy path in handleAutoMerge
// would otherwise fire, so this never delays a merge; it just lets GitHub
// finish the last mile. Returns true once armed, so the caller can stop
// without falling through to the direct-merge path.
//
// Gated on the same backoff as the direct-merge path: an arm attempt that
// itself keeps failing (bad credentials, a transient API error) must not be
// retried every poll tick either (#2450).
func (r *Handler) preflightArmNativeAutoMerge(ctx context.Context, t task.Task, issue github.PRIssue, backoff *github.AutoMergeBackoff, stateSig string) bool {
	if !r.nativeAutoMergeEnabled() || !NewMergeGate(issue.PR).ReadyToArm() {
		return false
	}
	if !backoff.ShouldAttempt(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA, stateSig) {
		metrics.AutoMergeAttempt(ctx, "suppressed", string(backoff.Class(issue.PR.Repository, issue.PR.Number)))
		return false
	}
	res := r.tryArmNativeAutoMerge(t, issue, "")
	if res.armed {
		r.clearMergeBackoff(ctx, issue.PR.Repository, issue.PR.Number)
		metrics.AutoMergeAttempt(ctx, "armed", "")
		r.evictReadyPRCache(issue.PR.Repository, issue.PR.Number)
		return true
	}
	if res.err != nil {
		metrics.AutoMergeAttempt(ctx, "attempted", "")
		class := github.ClassifyMergeError(res.err)
		if backoff.RecordFailure(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA, stateSig, class) {
			metrics.AutoMergeAttempt(ctx, "terminal", string(class))
		}
	}
	return false
}

func (r *Handler) handleAutoMerge(ctx context.Context, issue github.PRIssue) {
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.Type != project.ProjectTypePet {
		return
	}

	// Defense in depth: never merge a PR that lives outside the task's own
	// project. A mis-linked PR number (e.g. a branch-name collision) must not
	// be able to squash-merge an unrelated repo. proj.ID and PR.Repository are
	// both owner/repo.
	if issue.PR.Repository != proj.ID {
		r.logger.Warn("auto-merge.repo-mismatch",
			"task_id", t.ID, "task_project", proj.ID, "pr_repo", issue.PR.Repository, "pr", issue.PR.Number)
		return
	}

	backoff := r.mergeBackoff()
	gate := NewMergeGate(issue.PR)
	stateSig := gate.StateSignature()

	if r.preflightArmNativeAutoMerge(ctx, t, issue, backoff, stateSig) {
		return
	}

	renovateFix := slices.Contains(t.Tags, "renovate-fix")

	var ready bool
	var gateEvidence string
	if issue.PR.SourcedViaREST {
		if renovateFix {
			ready = gate.ReadyForMerge(MergePolicyRenovate, false)
			gateEvidence = "renovate_green"
		} else {
			ready = gate.ReadyForMerge(MergePolicyCopilot, false)
			gateEvidence = "approved"
		}
	} else {
		// Hold the merge until Copilot has reviewed and its threads are resolved.
		// Without this, a green PR merges on the first poll after CI passes —
		// before Copilot's (asynchronous) review lands — and its feedback is
		// skipped.
		//
		// Renovate dependency-bump PRs (surfaced via the "Fix CI" flow) are bot-
		// authored and never receive a Copilot review, so the Copilot gate would
		// strand them. The ReadyToMerge issue already implies green + mergeable +
		// !draft, so preserve their prior green auto-merge. Sybra-authored pet PRs
		// can also proceed after the owning task has completed Sybra's own review
		// stage, even when Copilot never comments.
		ready = renovateFix ||
			gate.ReadyForMerge(MergePolicyOwnBot, false) ||
			gate.ReadyForMerge(MergePolicyCopilot, false) ||
			gate.ReadyForMerge(MergePolicySybraReviewed, t.Reviewed)
	}
	if !ready {
		return
	}

	// Reprobe before spending another attempt: hold off on a direct-merge
	// retry against the same head SHA until this class's backoff window has
	// elapsed, instead of hammering an unresolved failure every poll tick
	// (#2450). A new push (different head SHA) always reprobes immediately.
	if !backoff.ShouldAttempt(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA, stateSig) {
		metrics.AutoMergeAttempt(ctx, "suppressed", string(backoff.Class(issue.PR.Repository, issue.PR.Number)))
		return
	}
	metrics.AutoMergeAttempt(ctx, "attempted", "")

	var mergeErr error
	if issue.PR.SourcedViaREST {
		merge := r.mergePRViaREST
		if merge == nil {
			merge = github.MergePRViaREST
		}
		mergeErr = merge(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA)
	} else {
		merge := r.mergePR
		if merge == nil {
			merge = github.MergePR
		}
		mergeErr = merge(issue.PR.Repository, issue.PR.Number)
	}
	r.evictReadyPRCache(issue.PR.Repository, issue.PR.Number)
	if mergeErr != nil {
		if r.nativeAutoMergeEnabled() && !issue.PR.SourcedViaREST && requiresNativeAutoMerge(mergeErr) {
			if res := r.tryArmNativeAutoMerge(t, issue, "direct_merge_rejected"); res.armed {
				r.clearMergeBackoff(ctx, issue.PR.Repository, issue.PR.Number)
				metrics.AutoMergeAttempt(ctx, "armed", "")
				return
			}
		}
		class := github.ClassifyMergeError(mergeErr)
		if backoff.RecordFailure(issue.PR.Repository, issue.PR.Number, issue.PR.HeadSHA, stateSig, class) {
			metrics.AutoMergeAttempt(ctx, "terminal", string(class))
		}
		r.logger.Error("auto-merge.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", mergeErr, "class", string(class))
		return
	}

	r.clearMergeBackoff(ctx, issue.PR.Repository, issue.PR.Number)
	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	auditData := map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	}
	if issue.PR.SourcedViaREST {
		auditData["sourced_via_rest"] = true
		auditData["gate_evidence"] = gateEvidence
		auditData["head_sha"] = issue.PR.HeadSHA
	}
	r.logAudit(audit.EventPRAutoMerged, t.ID, "", auditData)
	r.logger.Info("auto-merge.merged", "task_id", t.ID, "pr", issue.PR.Number)
	if r.onAutoMergeApplied != nil {
		r.onAutoMergeApplied()
	}
}

// mergeBackoff lazily initializes the handler's auto-merge backoff tracker.
// Mirrors prSnapshots's lazy-init pattern so tests that construct a bare
// Handler{} literal work without a dedicated constructor.
func (r *Handler) mergeBackoff() *github.AutoMergeBackoff {
	if r.autoMergeBackoff == nil {
		r.autoMergeBackoff = github.NewAutoMergeBackoff()
	}
	return r.autoMergeBackoff
}

// clearMergeBackoff drops backoff state for repo#number and, when a prior
// failure had been recorded there, records a "recovered" metric — a
// suppressed/failing PR that just succeeded, not a routine first-attempt
// merge.
func (r *Handler) clearMergeBackoff(ctx context.Context, repo string, number int) {
	if r.mergeBackoff().Clear(repo, number) {
		metrics.AutoMergeAttempt(ctx, "recovered", "")
	}
}

func (r *Handler) nativeAutoMergeEnabled() bool {
	return r.cfg != nil && r.cfg.GitHub.NativeAutoMerge
}

// tryArmNativeAutoMerge attempts to arm GitHub's native auto-merge. attempted
// reports whether a real GitHub call ran, while err carries the failure to
// classify/back off. Unsupported repo/branch stays a nil error so callers do
// not back it off like a genuine API failure.
func (r *Handler) tryArmNativeAutoMerge(t task.Task, issue github.PRIssue, fallback string) nativeAutoMergeAttemptResult {
	supportsFn := r.supportsAutoMergeFn
	if supportsFn == nil {
		supportsFn = github.SupportsNativeAutoMerge
	}
	ok, serr := supportsFn(issue.PR.Repository, issue.PR.BaseRefName)
	if serr != nil {
		r.logger.Error("auto-merge.native-support-check-failed", "task_id", t.ID, "pr", issue.PR.Number, "err", serr)
		return nativeAutoMergeAttemptResult{attempted: true, err: serr}
	}
	if !ok {
		return nativeAutoMergeAttemptResult{}
	}

	enableFn := r.enableAutoMergeFn
	if enableFn == nil {
		enableFn = github.EnableAutoMerge
	}
	if aerr := enableFn(issue.PR.Repository, issue.PR.Number); aerr != nil {
		r.logger.Error("auto-merge.native-arm-failed", "task_id", t.ID, "pr", issue.PR.Number, "err", aerr)
		return nativeAutoMergeAttemptResult{attempted: true, err: aerr}
	}

	r.prTracker.MarkHandled(t.ID, issue.Kind, issue.PR.HeadSHA)
	auditData := map[string]any{"pr": issue.PR.Number, "repo": issue.PR.Repository}
	if fallback != "" {
		auditData["fallback"] = fallback
	}
	r.logAudit(audit.EventAutoMergeEnabled, t.ID, "", auditData)
	if fallback != "" {
		r.logger.Info("auto-merge.native-armed", "task_id", t.ID, "pr", issue.PR.Number, "fallback", fallback)
	} else {
		r.logger.Info("auto-merge.native-armed", "task_id", t.ID, "pr", issue.PR.Number)
	}
	return nativeAutoMergeAttemptResult{armed: true, attempted: true}
}

func requiresNativeAutoMerge(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "base branch policy prohibits the merge") &&
		strings.Contains(msg, "--auto")
}

// escalateExhaustedFix parks a task whose pr-fix retry budget is spent. Trying
// the same fix MaxRetries times without clearing the issue means the agent
// can't resolve it on its own — flaky/unfixable CI, an unrebasable conflict, or
// review feedback that needs a human call — so stop looping and surface it.
// Mirrors the worktree circuit-breaker: own-PR tasks normally stay in In
// Review, but a hard, repeated failure escalates to human-required.
//
// Applies to every fixable kind (conflict, ci_failure, comments) — the durable
// retry cap keeps a capped entry across Cleanup, so a kind that did not escalate
// here would sit capped forever, never retried and never surfaced. Only the
// comments kind carries a feedback signature, so genuinely new reviewer feedback
// resets its budget (in Decide) before it ever reaches here. ready_to_merge
// never escalates — a green PR that simply hasn't merged is not a failure.
//
// Idempotent: a task already in human-required is left untouched. The tracker
// entry is cleared so a human un-parking the task starts from a fresh budget.
func (r *Handler) escalateExhaustedFix(issue github.PRIssue) {
	if issue.Kind == github.PRIssueReadyToMerge {
		return
	}
	if r.exhaustedFixIsFlaky(issue) {
		return
	}
	t, err := r.tasks.Get(issue.TaskID)
	if err != nil || t.Status == task.StatusHumanRequired || t.Status == task.StatusBlocked {
		return
	}
	maxRetries := r.prFixMaxRetries()
	reason := exhaustedFixReason(maxRetries, issue.Kind)
	tags := slices.DeleteFunc(slices.Clone(t.Tags), func(tag string) bool {
		return tag == reconciledLatchTag
	})
	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   issue.TaskID,
		ToStatus: task.StatusBlocked,
		Actor:    "review.pr-monitor.fix-exhausted.escalate",
		Extra: task.Update{
			StatusReason: task.Ptr(reason),
			Blocker: &blocker.State{
				Kind:       blocker.KindReviewFixExhausted,
				Actor:      blocker.ActorReview,
				Code:       string(issue.Kind),
				NextAction: "reprobe_pr",
				Exhausted:  true,
			},
			Tags: task.Ptr(tags),
		},
	}); err != nil {
		r.logger.Error("pr-monitor.fix-exhausted.escalate", "task_id", issue.TaskID, "err", err)
		return
	}
	r.prTracker.Clear(issue.TaskID, issue.Kind)
	r.logAudit(audit.EventPRFixExhausted, issue.TaskID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
		"kind": string(issue.Kind), "attempts": maxRetries,
	})
	r.logger.Warn("pr-monitor.fix-exhausted",
		"task_id", issue.TaskID, "pr", issue.PR.Number,
		"kind", string(issue.Kind), "attempts", maxRetries)
}

func (r *Handler) prFixMaxRetries() int {
	if r == nil || r.prTracker == nil {
		return github.MaxRetries
	}
	return r.prTracker.MaxRetries()
}

// exhaustedFixIsFlaky reports whether an exhausted ci_failure issue should
// stay in-review instead of parking human-required: flaky detection is
// enabled and ClassifyCIFlakiness attributes every currently-failing gating
// check on the head commit to flakiness rather than a deterministic bug.
// Logs EventPRCIFlakeDetected (cooldown-gated via prTracker's PRIssueCIFlake
// kind, so a still-flaky PR doesn't spam the audit log every poll cycle) as
// the observable record of why escalation was skipped. Only ci_failure
// carries a same-commit check history to classify — conflict/comments
// exhaustion always escalates. Fails closed: disabled detection, a missing
// PR repo/SHA, or a classifier error/deterministic verdict all return false.
func (r *Handler) exhaustedFixIsFlaky(issue github.PRIssue) bool {
	if issue.Kind != github.PRIssueCIFailure || r.cfg == nil || !r.cfg.GitHub.FlakyDetection {
		return false
	}
	if r.prTracker == nil || issue.PR.Repository == "" || issue.PR.HeadSHA == "" {
		return false
	}
	classify := r.classifyFlakiness
	if classify == nil {
		classify = github.ClassifyCIFlakiness
	}
	allFlaky, flakyChecks, err := classify(issue.PR.Repository, issue.PR.HeadSHA, r.cfg.GitHub.FlakyThreshold())
	if err != nil || !allFlaky {
		return false
	}
	if r.prTracker.ShouldHandle(issue.TaskID, github.PRIssueCIFlake, issue.PR.HeadSHA) {
		r.prTracker.MarkHandled(issue.TaskID, github.PRIssueCIFlake, issue.PR.HeadSHA)
		r.logAudit(audit.EventPRCIFlakeDetected, issue.TaskID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository, "checks": flakyChecks, "exhausted": true,
		})
		r.logger.Info("pr-monitor.ci-flake.exhausted-not-escalated",
			"task_id", issue.TaskID, "pr", issue.PR.Number, "checks", flakyChecks)
	}
	return true
}

// dispatchFlakyRerun classifies a lone ci_failure's gating checks against the
// head commit's full check-run history and, when every currently-failing
// gating check is flaky, triggers the same deterministic infra-rerun
// rerunCIFailure already performs (reusing its ciInfraRerunKind budget) but
// additionally records a distinct audit event so a flaky classification is
// observable separately from a blind rerun attempt. Returns false — a no-op —
// when flaky detection is disabled, the classifier errors or reports a
// deterministic failure, or the shared rerun budget for this head SHA is
// already spent; the caller then falls through to the ordinary
// rerun-then-fixer path unchanged, which is exactly today's behavior when
// this feature is off (the default).
func (r *Handler) dispatchFlakyRerun(t task.Task, issue github.PRIssue) bool {
	if r.cfg == nil || !r.cfg.GitHub.FlakyDetection || issue.PR.Repository == "" || issue.PR.HeadSHA == "" {
		return false
	}
	classify := r.classifyFlakiness
	if classify == nil {
		classify = github.ClassifyCIFlakiness
	}
	allFlaky, flakyChecks, err := classify(issue.PR.Repository, issue.PR.HeadSHA, r.cfg.GitHub.FlakyThreshold())
	if err != nil || !allFlaky {
		return false
	}
	if !r.rerunCIFailure(t, issue) {
		return false
	}
	r.prTracker.MarkHandled(t.ID, github.PRIssueCIFlake, issue.PR.HeadSHA)
	r.logAudit(audit.EventPRCIFlakeDetected, t.ID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository, "checks": flakyChecks,
	})
	r.logger.Info("pr-monitor.ci-flake.detected", "task_id", t.ID, "pr", issue.PR.Number, "checks", flakyChecks)
	return true
}

// handleFlakyCI handles a ci_failure issue classified as flaky (see
// github.flakyOnlyFailure: every failing workflow/check also shows a later
// successful rerun outcome on the same head SHA). It never dispatches a fix
// agent, since code changes do not fix noise, and never touches the
// deterministic ci_failure retry budget (handleTaskPRIssues routes a flaky
// issue around that budget entirely). It logs the pattern, then gives the
// check another shot through the same ci-infra rerun budget a deterministic
// ci_failure's first attempt uses (rerunCIFailure / ciInfraRerunKind). Only
// once that budget itself is exhausted, meaning reruns alone never cleared it,
// does it escalate to human-required, with a reason distinct from
// exhaustedFixReason so a human can tell "the fix agent gave up" apart from
// "this looks like a genuinely unstable test."
func (r *Handler) handleFlakyCI(issue github.PRIssue) {
	r.logAudit(audit.EventPRCIFlakyDetected, issue.TaskID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository, "head_sha": issue.PR.HeadSHA,
	})
	r.logger.Info("pr-monitor.ci-flaky-detected", "task_id", issue.TaskID, "pr", issue.PR.Number)

	t, err := r.tasks.Get(issue.TaskID)
	if err != nil || t.Status == task.StatusHumanRequired {
		return
	}

	if r.prTracker != nil && r.prTracker.AtCap(t.ID, ciInfraRerunKind) {
		maxRetries := r.prFixMaxRetries()
		tags := slices.DeleteFunc(slices.Clone(t.Tags), func(tag string) bool {
			return tag == reconciledLatchTag
		})
		if _, err := r.tasks.Apply(task.TransitionIntent{
			TaskID:   t.ID,
			ToStatus: task.StatusHumanRequired,
			Actor:    "review.pr-monitor.ci-flaky.escalate",
			Extra: task.Update{
				StatusReason: task.Ptr(persistentFlakyCIReason),
				Tags:         task.Ptr(tags),
			},
		}); err != nil {
			r.logger.Error("pr-monitor.ci-flaky.escalate", "task_id", t.ID, "err", err)
			return
		}
		r.prTracker.Clear(t.ID, ciInfraRerunKind)
		r.logAudit(audit.EventPRFixExhausted, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
			"kind": string(ciInfraRerunKind), "attempts": maxRetries,
		})
		r.logger.Warn("pr-monitor.ci-flaky.exhausted", "task_id", t.ID, "pr", issue.PR.Number)
		return
	}

	r.rerunCIFailure(t, issue)
}

// ciFailurePrompt is the pr-fix agent prompt for a failing-CI issue.
func ciFailurePrompt(pr github.PullRequest) string {
	return fmt.Sprintf(
		"Fix failing CI on branch `%s` (PR #%d). "+
			"Check the failing run with `gh run view --log-failed`, "+
			"fix the code, commit and push. No unrelated changes.\n\n"+
			"%s\n\n%s\n\n%s",
		pr.HeadRefName, pr.Number,
		ciFailureDiagnosisRules(pr.BaseRefName),
		prFixTamperingRules,
		prFixPushPrompt(pr.HeadRefName, "Push to the same remote create-pr would target for this worktree:", true, false),
	)
}

// Without a base-branch baseline agents blame the diff for infra/flake failures. EXC:FILE011:load-bearing-invariant
func ciFailureDiagnosisRules(baseRef string) string {
	base := "the base branch"
	if baseRef != "" {
		base = "`" + baseRef + "`"
	}
	return "Diagnose before you change anything:\n" +
		"- Identify which jobs failed and why. A job that fails during setup, " +
		"provisioning, or image pull is infrastructure, not your diff.\n" +
		"- Before changing ANY code for a test failure, check whether the same job " +
		"already fails on " + base + " (e.g. `gh run list --branch <base> " +
		"--workflow <workflow> --limit 20`). If it does, the failure is not yours: " +
		"report `flake` with that evidence.\n" +
		"- State the causal link before you edit: how does this diff produce this " +
		"exact failure? If you cannot answer that, you do not have a fix — report " +
		"`flake` or `human-required` instead of guessing.\n" +
		"- If the failure is transient and unrelated, re-run the failed jobs with " +
		"`gh run rerun <run-id> --failed` and report `flake`. Never mint an empty " +
		"or amended commit just to retrigger CI."
}

// Banning only test tampering redirects the pressure onto production defaults. EXC:FILE011:load-bearing-invariant
const prFixTamperingRules = "Never weaken, skip, delete, or hardcode tests, " +
	"snapshots, or fixtures to make CI pass, and never edit CI config to neuter a " +
	"gate — fix the underlying code. Tampering is detected and blocks the task.\n\n" +
	"The same prohibition covers product code: never change production defaults, " +
	"timeouts, intervals, or retry counts to make a test pass. A failing or flaky " +
	"test is never by itself evidence for a product-code change — only a " +
	"demonstrated causal link is. Shortening an interval or widening a timeout so " +
	"a suite goes green is tampering with a user-facing default, not a fix.\n\n" +
	"Never force-push and never rewrite already-pushed history (`--force`, " +
	"`--force-with-lease`, `commit --amend`, rebase onto a pushed base). Append a " +
	"new commit instead; a force-push can destroy work this PR depends on."

func prFixLiveStateGuard(pr github.PullRequest) string {
	repoFlag := ""
	if repo := strings.TrimSpace(pr.Repository); repo != "" {
		repoFlag = " --repo " + repo
	}
	return fmt.Sprintf(
		"Before any \"already fixed\" / \"already merged\" / \"no work needed\" conclusion, re-check the LIVE PR state:\n"+
			"```sh\n"+
			"LIVE_BASE=$(gh pr view %d%s --json baseRefName --jq .baseRefName)\n"+
			"git fetch origin \"+refs/heads/$LIVE_BASE:refs/remotes/origin/$LIVE_BASE\"\n"+
			"gh pr view %d%s --json state,mergeable,baseRefName\n"+
			"```\n\n"+
			"- Do not trust NOTES.md, a prior run's recorded merge commit, or an unchanged local worktree as proof the PR is still mergeable.\n"+
			"- Treat GitHub's current `mergeable` value plus the freshly fetched `refs/remotes/origin/$LIVE_BASE` as the source of truth.\n"+
			"- If GitHub says `mergeable=CONFLICTING`, or the refreshed base branch creates a new conflict, resolve that live conflict now instead of reporting `continue` or \"no work needed\" from stale local state.\n"+
			"- If the live probe shows the PR no longer needs a code change from this run, stop without pushing and report `SYBRA_PR_FIX_RESULT: human-required` with a short reason describing the live remote state.",
		pr.Number, repoFlag, pr.Number, repoFlag,
	)
}

// prFixPushPrompt renders the create-pr-equivalent push snippet for a pr-fix
// agent. When fenced is true it emits a standalone ```sh block (optionally
// preceded by intro); when false it emits only the command lines so the caller
// can splice them into an already-open code fence without nesting.
//
// allowHistoryRewrite is only for no-PR recovery paths, where a rebase and
// force-with-lease are safe because no external PR depends on the branch shape
// yet.
func prFixPushPrompt(branch, intro string, fenced, allowHistoryRewrite bool) string {
	return prFixPushPromptWithRemote(branch, intro, fenced, allowHistoryRewrite, "")
}

func prFixPushPromptWithRemote(branch, intro string, fenced, allowHistoryRewrite bool, remote string) string {
	var b strings.Builder
	if fenced && intro != "" {
		b.WriteString(intro)
		b.WriteByte('\n')
	}
	if fenced {
		b.WriteString("```sh\n")
	}
	if remote == "" {
		b.WriteString("PUSH_REMOTE=origin\n")
		b.WriteString("if git config --get remote.fork.url >/dev/null; then PUSH_REMOTE=fork; fi\n")
	} else {
		fmt.Fprintf(&b, "PUSH_REMOTE=%s\n", remote)
	}
	b.WriteString("PREFLIGHT_REF=HEAD:refs/heads/sybra-preflight/$(git rev-parse --verify HEAD)\n")
	b.WriteString("git push --dry-run \"$PUSH_REMOTE\" \"$PREFLIGHT_REF\"\n")
	fmt.Fprintf(&b, "git push \"$PUSH_REMOTE\" HEAD:%s", branch)
	if allowHistoryRewrite {
		fmt.Fprintf(&b, "\n# If you rebased or otherwise rewrote this branch's history, use lease-protected force-push instead.\ngit push --force-with-lease \"$PUSH_REMOTE\" HEAD:%s", branch)
	}
	if fenced {
		b.WriteString("\n```")
	}
	return b.String()
}

// prIssueBody returns the pr-fix agent prompt for one fixable issue kind. ok is
// false for kinds with no agent prompt (ready_to_merge), which never reach the
// dispatch path.
func prIssueBody(ctx context.Context, issue github.PRIssue) (string, bool) {
	switch issue.Kind {
	case github.PRIssueConflict:
		return conflictPrompt(ctx, issue.PR), true
	case github.PRIssueCIFailure:
		return ciFailurePrompt(issue.PR), true
	case github.PRIssueComments:
		return commentsPrompt(ctx, issue.PR), true
	default:
		return "", false
	}
}

// fixKindPriority orders fixable kinds for coalesced dispatch. The kind with
// the lowest numeric priority (i.e. highest priority) becomes the "primary":
// it drives worktree prep, the workflow's pr_issue_kind var, and cancel/phase
// reconciliation. Conflicts sort first so a
// conflicting PR checks out its branch WITHOUT rebasing (PrepareForFix), then
// comments so any pair that includes review feedback also prefers the
// branch-preserving checkout; a lone ci_failure keeps its rebasing PrepareForTask
// path unchanged.
func fixKindPriority(kind github.PRIssueKind) int {
	switch kind {
	case github.PRIssueConflict:
		return 0
	case github.PRIssueComments:
		return 1
	case github.PRIssueCIFailure:
		return 2
	default:
		return 3
	}
}

func fixKindLabel(kind github.PRIssueKind) string {
	switch kind {
	case github.PRIssueConflict:
		return "Merge conflicts"
	case github.PRIssueCIFailure:
		return "Failing CI"
	case github.PRIssueComments:
		return "Review comments"
	default:
		return string(kind)
	}
}

func (r *Handler) logPRIssueDetected(taskID string, issue github.PRIssue) {
	var event string
	switch issue.Kind {
	case github.PRIssueConflict:
		event = audit.EventPRConflictDetected
	case github.PRIssueCIFailure:
		event = audit.EventPRCIFailureDetected
	case github.PRIssueComments:
		event = audit.EventPRCommentsDetected
	default:
		return
	}
	r.logAudit(event, taskID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository,
	})
}

// coalescedFixPrompt composes a single pr-fix agent prompt covering every issue
// the monitor wants fixed on a PR this cycle. A single issue yields its bare
// per-kind prompt (behavior unchanged); multiple issues — e.g. a push that both
// fails CI and drew review comments — are stitched into one pass so exactly one
// agent runs per review round instead of one agent per kind across cycles.
//
// holdSuffix (from reviewHoldFixSuffix) is appended once at the end when the
// review-hold setting is on AND the set includes a review-comments issue — the
// only kind that posts thread replies. It overrides the "reply live and push"
// instructions above, so it must land after them.
func coalescedFixPrompt(ctx context.Context, issues []github.PRIssue, holdSuffix string) string {
	var prompt string
	if len(issues) == 1 {
		prompt, _ = prIssueBody(ctx, issues[0])
	} else {
		var b strings.Builder
		b.WriteString("This PR has multiple open issues from the same push. Address " +
			"ALL of them in one pass, then push once at the end (the per-section push " +
			"commands are equivalent — run it a single time).\n\n")
		for i := range issues {
			body, ok := prIssueBody(ctx, issues[i])
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "=== Issue %d: %s ===\n%s\n\n", i+1, fixKindLabel(issues[i].Kind), body)
		}
		prompt = strings.TrimRight(b.String(), "\n")
	}
	if len(issues) > 0 {
		prompt += "\n\n" + prFixLiveStateGuard(issues[0].PR)
	}
	if holdSuffix != "" && slices.ContainsFunc(issues, func(i github.PRIssue) bool {
		return i.Kind == github.PRIssueComments
	}) {
		prompt += holdSuffix
	}
	if slices.ContainsFunc(issues, func(i github.PRIssue) bool {
		return prHasCurrentApproval(i.PR)
	}) {
		prompt += "\n\nApproval preservation:\n" +
			"- This PR is already approved. Do not merge/rebase/update the base branch or push a clean/base-only merge that only changes the merge-base.\n" +
			"- Push only if you make a substantive code, test, or documentation fix for the failing CI, conflict, or review feedback.\n" +
			"- If no substantive fix is needed, stop without pushing and report `SYBRA_PR_FIX_RESULT: human-required` with the reason."
	}
	return prompt
}

func (r *Handler) handlePRIssueReplacingWorkflow(ctx context.Context, issue github.PRIssue, cancelReason string) bool {
	return r.dispatchFixIssuesWithOptions(ctx, issue.TaskID, []github.PRIssue{issue}, dispatchFixOptions{
		replaceActiveWorkflow: true,
		cancelReason:          cancelReason,
	})
}

// dispatchFixIssues spawns a single pr-fix agent that addresses every handled
// issue for a task. handle must be non-empty and contain only fixable kinds
// (conflict, ci_failure, comments); the caller (handleTaskPRIssues) filters out
// ready_to_merge and applies the retry/cooldown gate. Coalescing avoids the
// double-dispatch where a CI failure and review comments from the same push each
// spawned their own sequential agent.
func (r *Handler) dispatchFixIssues(ctx context.Context, taskID string, handle []github.PRIssue) bool {
	return r.dispatchFixIssuesWithOptions(ctx, taskID, handle, dispatchFixOptions{})
}

func (r *Handler) dispatchFixIssuesWithOptions(ctx context.Context, taskID string, handle []github.PRIssue, opts dispatchFixOptions) bool {
	if len(handle) == 0 {
		return false
	}
	t, err := r.tasks.Get(taskID)
	if err != nil {
		return false
	}
	// Stable-sort by fix priority so the primary (index 0) drives worktree prep
	// and the prompt reads in execution order (conflicts → comments → CI).
	slices.SortStableFunc(handle, func(a, b github.PRIssue) int {
		return fixKindPriority(a.Kind) - fixKindPriority(b.Kind)
	})
	for i := range handle {
		r.logPRIssueDetected(t.ID, handle[i])
	}
	primary := handle[0]

	// Early mutation guard: a workflow may already be active for this task
	// (e.g. an in-flight pr-fix step) — never let the deterministic fast-path
	// or a fresh worktree prep race it. handleTaskPRIssues already applies this
	// gate for its own callers, but dispatchFixIssues is also reached via
	// handlePRIssueReplacingWorkflow/RecoverStaleBranchConflict, so it is
	// repeated here.
	if r.WorkflowEngine != nil && r.WorkflowEngine.HasActiveWorkflow(t.ID) && !opts.replaceActiveWorkflow {
		return false
	}

	if !opts.replaceActiveWorkflow &&
		len(handle) == 1 &&
		primary.Kind == github.PRIssueCIFailure &&
		(r.dispatchFlakyRerun(t, primary) || r.rerunCIFailure(t, primary)) {
		return true
	}

	dir, ok := r.prepareWorktree(ctx, t, primary)
	if !ok {
		return false
	}

	if admit, handled := r.preflightPushCredentials(ctx, t.ID, dir); !admit {
		return handled
	}

	singleConflict := len(handle) == 1 && handle[0].Kind == github.PRIssueConflict
	autoResolveAdmitted := !opts.replaceActiveWorkflow &&
		r.cfg != nil && r.cfg.GitHub.AutoResolveCleanMerges &&
		!prHasCurrentApproval(primary.PR)

	if autoResolveAdmitted && singleConflict && r.autoResolveConflict(ctx, t, primary.PR, dir) {
		return true
	}

	// Checked after the fast paths: the budget caps LLM agents, not deterministic work. EXC:FILE011:load-bearing-invariant
	if !opts.replaceActiveWorkflow && r.durableFixBudgetSpent(t.ID, primary.PR.HeadSHA) {
		r.escalateExhaustedFix(primary)
		return true
	}

	// dispatchPRIssueWithOptions -> WorkflowEngine.DispatchEvent eventually
	// reaches execShell, which derives its context from workflow.Engine's own
	// e.ctx field (Engine.SetContext), not an explicit parameter threaded here.
	// contextcheck no longer flags this call site (verified with a clean
	// build+lint cache), so no suppression directive is needed here.
	return r.dispatchPRIssueWithOptions(ctx, t, primary, handle, coalescedFixPrompt(ctx, handle, reviewHoldFixSuffix(r.cfg)), dir, opts)
}

func prHasCurrentApproval(pr github.PullRequest) bool {
	return pr.ReviewDecision == "APPROVED" || pr.RESTApproved
}

func (r *Handler) preflightPushCredentials(ctx context.Context, taskID, dir string) (admit, handled bool) {
	preflight := r.pushPreflightFn
	if preflight == nil {
		preflight = project.PreflightPushCredentials
	}
	if err := preflight(ctx, dir); err != nil {
		reason := "GitHub push credential preflight failed before starting PR fix: " + truncatePushPreflightReason(err.Error(), 240)
		if _, updateErr := r.tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusHumanRequired,
			Actor:    "review.pr-monitor.push-preflight",
			Extra: task.Update{
				StatusReason: task.Ptr(reason),
			},
		}); updateErr != nil {
			r.logger.Error("pr-monitor.push-preflight.status", "task_id", taskID, "err", updateErr)
			return false, false
		}
		r.logger.Warn("pr-monitor.push-preflight.failed", "task_id", taskID, "err", err)
		return false, true
	}
	return true, false
}

func truncatePushPreflightReason(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	if limit <= 0 || len(s) <= limit {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if b.Len()+utf8.RuneLen(r) > limit {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String()) + "..."
}

// A rerun re-runs jobs the repo already ran and sends no content anywhere, so the work/pet split does not apply. EXC:FILE011:load-bearing-invariant
func (r *Handler) rerunCIFailure(t task.Task, issue github.PRIssue) bool {
	if r.projects == nil || r.prTracker == nil || t.ProjectID == "" || issue.PR.Number <= 0 {
		return false
	}
	if _, err := r.projects.Get(t.ProjectID); err != nil {
		return false
	}
	shaGate := issue.PR.HeadSHA
	if issue.PR.CIFlaky {
		// Flaky reruns do not produce a new commit; cap by attempt count, not SHA.
		shaGate = ""
	}
	if !r.prTracker.ShouldHandle(t.ID, ciInfraRerunKind, shaGate) {
		return false
	}

	rerun := r.rerunFailedChecks
	if rerun == nil {
		rerun = github.RerunFailedChecks
	}
	if err := rerun(issue.PR.Repository, issue.PR.Number); err != nil {
		if isRerunPermissionDenied(err) {
			if _, updateErr := r.tasks.Apply(task.TransitionIntent{
				TaskID:   t.ID,
				ToStatus: task.StatusHumanRequired,
				Actor:    "review.pr-monitor.ci-rerun.permission",
				Extra: task.Update{
					StatusReason: task.Ptr(ciInfraRerunPermissionReason),
				},
			}); updateErr != nil {
				r.logger.Error("pr-monitor.ci-rerun.permission-status",
					"task_id", t.ID, "pr", issue.PR.Number, "err", updateErr)
				return false
			}
			r.logger.Warn("pr-monitor.ci-rerun.permission-denied",
				"task_id", t.ID, "pr", issue.PR.Number, "err", err)
			return true
		}
		r.logger.Warn("pr-monitor.ci-rerun.failed", "task_id", t.ID, "pr", issue.PR.Number, "err", err)
		return false
	}

	r.prTracker.MarkHandled(t.ID, ciInfraRerunKind, issue.PR.HeadSHA)
	r.evictReadyPRCache(issue.PR.Repository, issue.PR.Number)
	r.logAudit(audit.EventPRCIFailureRerun, t.ID, "", map[string]any{
		"pr": issue.PR.Number, "repo": issue.PR.Repository, "head_sha": issue.PR.HeadSHA,
	})
	r.logger.Info("pr-monitor.ci-rerun.started", "task_id", t.ID, "pr", issue.PR.Number)
	return true
}

func isRerunPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "resource not accessible by integration")
}

// autoResolveConflict attempts the deterministic clean-merge fast-path for a
// single conflict-only PR issue: fetch the base branch, run a real git merge
// via tryCleanMergeFn, and — only when that merge creates a new commit with no
// conflicting hunks — push it via pushSyncFn and mark the issue handled.
// Returns false for a conflicting merge, a no-op merge (branch was already up
// to date — nothing to push, so the agent still needs to look at why GitHub
// reports a conflict), or any fetch/merge/push error; the caller then falls
// through to the agent-assisted recovery path unchanged.
func (r *Handler) autoResolveConflict(ctx context.Context, t task.Task, pr github.PullRequest, dir string) bool {
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.Type != project.ProjectTypePet {
		return false
	}

	base, err := resolveAutoResolveBase(ctx, dir, pr, proj)
	if err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.base", "task_id", t.ID, "pr", pr.Number, "err", err)
		return false
	}

	branch, err := project.CurrentBranch(ctx, dir)
	if err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.branch", "task_id", t.ID, "pr", pr.Number, "err", err)
		return false
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		r.logger.Warn("pr-monitor.auto-resolve.branch-empty", "task_id", t.ID, "pr", pr.Number)
		return false
	}

	if err := project.ReconcileWithRemote(ctx, dir, branch); err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.branch-preflight", "task_id", t.ID, "pr", pr.Number, "branch", branch, "err", err)
		return false
	}

	preMergeHead, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "rev-parse", "--verify", "HEAD")
	if err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.pre-merge-head", "task_id", t.ID, "pr", pr.Number, "err", err)
		return false
	}
	preMergeHead = strings.TrimSpace(preMergeHead)

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base)
	if err := gitexec.Run(ctx, gitexec.Options{Dir: dir}, "fetch", "origin", refspec); err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.fetch", "task_id", t.ID, "pr", pr.Number, "err", err)
		return false
	}

	mergeFn := r.tryCleanMergeFn
	if mergeFn == nil {
		mergeFn = project.TryCleanMerge
	}
	result, err := mergeFn(ctx, dir, "refs/remotes/origin/"+base)
	if err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.merge", "task_id", t.ID, "pr", pr.Number, "err", err)
		return false
	}
	if result != project.CleanMergeCreated {
		return false
	}

	mergedHead, err := gitexec.Output(ctx, gitexec.Options{Dir: dir}, "rev-parse", "--verify", "HEAD")
	if err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.post-merge-head", "task_id", t.ID, "pr", pr.Number, "err", err)
		r.rollbackAutoResolvedMerge(ctx, t.ID, pr.Number, dir, preMergeHead, "post-merge-head")
		return false
	}
	mergedHead = strings.TrimSpace(mergedHead)
	if mergedHead == "" {
		r.logger.Warn("pr-monitor.auto-resolve.post-merge-head-empty", "task_id", t.ID, "pr", pr.Number)
		r.rollbackAutoResolvedMerge(ctx, t.ID, pr.Number, dir, preMergeHead, "post-merge-head-empty")
		return false
	}

	pushFn := r.pushSyncFn
	if pushFn == nil {
		pushFn = project.PushSync
	}
	if err := pushFn(ctx, dir, branch); err != nil {
		r.logger.Warn("pr-monitor.auto-resolve.push", "task_id", t.ID, "pr", pr.Number, "err", err)
		r.rollbackAutoResolvedMerge(ctx, t.ID, pr.Number, dir, preMergeHead, "push")
		return false
	}

	pr.HeadSHA = mergedHead
	r.prTracker.MarkHandled(t.ID, github.PRIssueConflict, r.prIssueDispatchSHA(ctx, github.PRIssue{
		Kind:   github.PRIssueConflict,
		TaskID: t.ID,
		PR:     pr,
	}))
	r.evictReadyPRCache(pr.Repository, pr.Number)
	r.logAudit(audit.EventPRConflictAutoResolved, t.ID, "", map[string]any{
		"pr": pr.Number, "issue": string(github.PRIssueConflict),
	})
	r.logger.Info("pr-monitor.auto-resolved", "task_id", t.ID, "pr", pr.Number)
	return true
}

func resolveAutoResolveBase(ctx context.Context, dir string, pr github.PullRequest, proj project.Project) (string, error) {
	base := pr.BaseRefName
	if base == "" {
		db, err := project.DefaultBranch(ctx, proj.ClonePath)
		if err != nil {
			return "", fmt.Errorf("resolve default branch for %s: %w", proj.ID, err)
		}
		base = strings.TrimSpace(db)
	}
	if base == "" {
		return "", errors.New("base branch is empty")
	}
	if err := gitexec.Run(ctx, gitexec.Options{Dir: dir}, "check-ref-format", "--branch", base); err != nil {
		return "", fmt.Errorf("validate base branch %q: %w", base, err)
	}
	return base, nil
}

func (r *Handler) rollbackAutoResolvedMerge(ctx context.Context, taskID string, prNumber int, dir, preMergeHead, reason string) {
	if preMergeHead == "" {
		return
	}
	resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := gitexec.Run(resetCtx, gitexec.Options{Dir: dir}, "reset", "--hard", preMergeHead); err != nil {
		r.logger.Error("pr-monitor.auto-resolve.rollback-failed",
			"task_id", taskID, "pr", prNumber, "reason", reason, "pre_merge_head", preMergeHead, "err", err)
		return
	}
	r.logger.Info("pr-monitor.auto-resolve.rolled-back",
		"task_id", taskID, "pr", prNumber, "reason", reason, "pre_merge_head", preMergeHead)
}

// dispatchPRIssueWithOptions starts the pr-fix workflow for primary and, on
// success, marks every coalesced issue in handle as handled so none re-fires
// next cycle. The primary kind is authoritative for the workflow's
// pr_issue_kind var (cancel and phase reconciliation key on it); handle
// carries the full set for the retry tracker.
func (r *Handler) dispatchPRIssueWithOptions(ctx context.Context, t task.Task, primary github.PRIssue, handle []github.PRIssue, prompt, dir string, opts dispatchFixOptions) bool {
	if r.WorkflowEngine == nil {
		r.logger.Error("pr-monitor.no-workflow-engine", "task_id", t.ID)
		return false
	}

	// Dispatch pr.event through the engine so trigger conditions in the
	// workflow YAML stay authoritative. StartWorkflow would bypass them.
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s%s", t.Title, prompt, PRFixResultContract)
	kinds := make([]string, 0, len(handle))
	for i := range handle {
		kinds = append(kinds, string(handle[i].Kind))
	}
	vars := map[string]string{
		"prompt":                fullPrompt,
		"pr_issue_kind":         string(primary.Kind),
		"pr_issue_kinds":        strings.Join(kinds, ","),
		workflow.WorkflowVarDir: dir,
		// Exposed separately (not just baked into fullPrompt above) so the
		// test_fix step's own YAML-authored prompt can reuse the identical
		// result contract instead of duplicating it as a second copy that
		// could drift out of sync.
		"pr_fix_result_contract": PRFixResultContract,
		// Same reasoning: the test_fix step's static YAML prompt needs the
		// host-appropriate commit flags (-s vs -s -S) for its own commit
		// instruction, computed once here rather than hardcoded in the YAML.
		"commit_sign_flags": project.CommitSignFlags(ctx),
	}
	// Deterministic backstop for review-hold: when the hold is active and this
	// fix touches review comments, the agent drafted its replies into a pending
	// review, so route_pr_fix_result must park the task for a human regardless of
	// the agent's terminal sentinel (in push mode it pushed and would otherwise
	// emit `continue`). Relying on the prompt sentinel alone is unsafe — the
	// PRFixResultContract appended after the hold suffix re-introduces `continue`.
	if r.cfg.ReviewHoldEnabled() && slices.ContainsFunc(handle, func(i github.PRIssue) bool {
		return i.Kind == github.PRIssueComments
	}) {
		vars[workflow.ReviewHoldParkVar] = "true"
	}
	extraFields := map[string]string{"pr.issue_kind": string(primary.Kind)}
	var (
		wfID string
		err  error
	)
	if opts.replaceActiveWorkflow {
		wfID, err = r.WorkflowEngine.ReplaceWorkflowForEvent(t.ID, "pr.event", extraFields, vars, opts.cancelReason)
	} else {
		wfID, err = r.WorkflowEngine.DispatchEvent(t.ID, "pr.event", extraFields, vars)
	}
	if err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			r.logger.Info("pr-monitor.workflow-already-active",
				"task_id", t.ID, "kind", string(primary.Kind))
			return false
		}
		if r.recoverRetryablePRFixDispatch(t.ID, err) {
			r.logger.Info("pr-monitor.workflow-dispatch-parked-retry",
				"task_id", t.ID, "kind", string(primary.Kind), "err", err)
			return true
		}
		r.logger.Error("pr-monitor.workflow-dispatch", "task_id", t.ID, "err", err)
		return false
	}
	if wfID == "" {
		r.logger.Warn("pr-monitor.no-matching-workflow",
			"task_id", t.ID, "kind", string(primary.Kind))
		return false
	}

	for i := range handle {
		r.prTracker.MarkHandled(t.ID, handle[i].Kind, r.prIssueDispatchSHA(ctx, handle[i]))
	}
	r.logAudit(audit.EventPRFixAgentStarted, t.ID, "", map[string]any{
		"issue": string(primary.Kind), "kinds": strings.Join(kinds, ","),
		"pr": primary.PR.Number, "workflow": wfID,
	})

	r.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(primary.Kind), "kinds", strings.Join(kinds, ","),
		"pr", primary.PR.Number, "workflow", wfID,
	)
	return true
}

// markConflictRecoveryExhausted records, on the task itself, that autonomous
// branch-conflict recovery was attempted and gave up rather than declining
// silently. Without this, agentorch.MarkRebaseBlocked's caller-side fallback
// writes its generic worktreeerr.RebaseBlockedReason over whatever is here,
// leaving an operator (or the automated human-review agent reading a stale
// "recovered" log line) unable to tell an exhausted retry budget apart from
// recovery that is still in progress. MarkRebaseBlocked checks for this
// specific status+non-empty-reason combination before overwriting it, so this
// must run before returning false to the caller.
func (r *Handler) markConflictRecoveryExhausted(taskID string, kind github.PRIssueKind) {
	attempts := r.prTracker.Retries(taskID, kind)
	reason := fmt.Sprintf(
		"branch conflict recovery attempted %d time(s) and failed: resolve conflicts or recreate the task branch",
		attempts)
	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   taskID,
		ToStatus: task.StatusHumanRequired,
		Actor:    "review.pr-monitor.branch-conflict.exhausted",
		Extra: task.Update{
			StatusReason: task.Ptr(reason),
		},
	}); err != nil {
		r.logger.Error("pr-monitor.branch-conflict.exhausted-status", "task_id", taskID, "err", err)
	}
}

// RecoverStaleBranchConflict turns a worktree-prep rebase failure into
// autonomous conflict resolution instead of a human escalation. The CI-fix and
// implement/review/test prepare paths rebase the task branch onto base before
// the agent starts; when the branch also conflicts with base — common when
// GitHub still reports UNKNOWN mergeability, so the monitor only emitted the CI
// failure, not a conflict — that rebase aborts. Rather than stranding a human,
// dispatch the conflict pr-fix, which checks out the PR head WITHOUT rebasing
// and has the agent resolve conflicts itself.
//
// Returns false (caller escalates to human as before) when there is no linked
// PR to fix, the PR is closed/unfetchable, or the conflict-fix retry budget is
// already spent. handlePRIssueReplacingWorkflow's conflict branch prepares via
// PrepareForFix (no rebase), so this never re-enters the rebasing path that
// called it.
//
// Guards against the pool-exhaustion livelock: when a prior call already
// dispatched pr-fix's conflict fix and it is merely parked waiting for a
// dispatch/pool slot (ErrAgentPoolBusy et al — see WorkflowParkedWaiting), a
// re-entrant caller must leave it alone rather than cancel and restart it.
// Without this, every rebase-block re-probe (e.g. a CI-fix worktree prep
// re-hitting the same unresolved divergence) cancels the in-flight pr-fix
// workflow and redispatches a fresh one that immediately re-parks on the
// still-exhausted pool, burning a full worktree rebuild+setup per cycle
// without ever actually attempting conflict resolution. ResumeStalled is the
// only thing that should re-drive a parked step once a slot frees.
func (r *Handler) RecoverStaleBranchConflict(taskID string) bool {
	if r == nil || r.WorkflowEngine == nil || r.prTracker == nil {
		return false
	}
	t, err := r.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return false
	}
	if r.WorkflowEngine.WorkflowParkedWaiting(taskID, branchConflictFixWorkflowID) ||
		r.prFixParkedOnConflict(taskID) {
		r.logger.Info("pr-monitor.branch-conflict.already-parked-waiting", "task_id", taskID)
		return true
	}
	// No PR yet (still in implementation/review/testing, or at create_pr) —
	// there is no PR to check out via the PR-numbered path below. Resolve the
	// task's own branch by name instead. This never re-enters the PR-numbered
	// branch: recoverBranchConflictNoPR has its own return, so the code below
	// (and its byte-for-byte behavior for t.PRNumber != 0) is unreachable from
	// this branch.
	if t.PRNumber == 0 {
		return r.recoverBranchConflictNoPR(t)
	}
	// Don't loop forever on a genuinely unresolvable conflict — once the
	// conflict-fix budget is spent the normal exhaustion path escalates.
	if r.prTracker.AtCap(taskID, github.PRIssueConflict) {
		r.markConflictRecoveryExhausted(taskID, github.PRIssueConflict)
		return false
	}
	fetchFn := github.FetchPRForMonitor
	if r.fetchKnownPRFn != nil {
		fetchFn = r.fetchKnownPRFn
	}
	pr, open, ferr := fetchFn(t.ProjectID, t.PRNumber)
	if ferr != nil || !open {
		return false
	}
	r.logger.Info("pr-monitor.rebase-block.recover-as-conflict",
		"task_id", taskID, "pr", t.PRNumber)
	// context.Background() is a dead end here: RecoverStaleBranchConflict is
	// wired as agentorch.Orchestrator.ConflictRecovery, a fixed func(taskID string)
	// bool callback (see internal/sybra/agentorch) with no ctx parameter to thread from.
	return r.handlePRIssueReplacingWorkflow(context.Background(),
		github.PRIssue{TaskID: taskID, Kind: github.PRIssueConflict, PR: pr},
		"rebase conflict recovery")
}

// prFixParkedOnConflict reports whether the task's active workflow is a pr-fix
// parked mid-run that was dispatched for a conflict. pr-fix is not
// conflict-only — it also handles ci_failure and comments — so only a conflict
// pr-fix means autonomous conflict recovery is already in flight and may
// short-circuit RecoverStaleBranchConflict. A pr-fix parked on ci_failure or
// comments must NOT suppress dispatching conflict recovery for a real rebase
// conflict; treating it as in-flight would report success without ever
// resolving the conflict, and the caller would skip human escalation.
func (r *Handler) prFixParkedOnConflict(taskID string) bool {
	vars, ok := r.WorkflowEngine.WorkflowParkedWaitingVars(taskID, prFixWorkflowID)
	if !ok {
		return false
	}
	return slices.Contains(coalescedWorkflowKinds(vars), string(github.PRIssueConflict))
}

// recoverBranchConflictNoPR is the no-PR sibling of RecoverStaleBranchConflict:
// the task has no linked PR yet (still in implementation/review/testing, or
// at create_pr), so there is no PR head to check out. Instead it resolves the
// task's OWN branch by name (PrepareForBranchFix), resolves the branch via a
// bounded headless pr-fix sub-run (branch-conflict-fix workflow), and on
// success resumes the task's original interrupted workflow/stage rather than
// jumping to a terminal status.
//
// Guards fail closed exactly like the PR-numbered path: missing dependencies
// and exhausted no-PR branch-conflict retry budgets return false so the caller
// (agentorch.MarkRebaseBlocked helper) escalates to human-required. An
// already-in-flight recovery is treated as handled, so a concurrent caller
// cannot cancel the workflow the first caller is preparing. Never loops:
// a conflict discovered while resolving THIS conflict re-enters
// agentorch.MarkRebaseBlocked -> RecoverStaleBranchConflict -> here, and the in-flight
// marker (still held from the outer call, since this method runs
// synchronously start to finish) makes the re-entrant call bail out
// immediately.
func (r *Handler) recoverBranchConflictNoPR(t task.Task) bool {
	return r.recoverTaskBranchConflict(context.Background(), t, taskBranchConflictRecoverySpec{
		retryKind:     branchConflictRetryKind,
		allowRecreate: true,
		prompt:        branchConflictPrompt,
	})
}

func (r *Handler) recoverSameBranchConflict(ctx context.Context, t task.Task, branch, remote string, trustedOrigin bool) bool {
	if remote == "" {
		remote = "origin"
	}
	return r.recoverTaskBranchConflict(ctx, t, taskBranchConflictRecoverySpec{
		retryKind:      sameBranchConflictRetryKind,
		branchOverride: branch,
		remoteOverride: remote,
		trustedOrigin:  trustedOrigin && remote == "origin",
		prompt: func(ctx context.Context, t task.Task, _ string) string {
			return sameBranchConflictPrompt(ctx, t, remote)
		},
	})
}

func (r *Handler) sameBranchConflictRemote(ctx context.Context, t task.Task, pr github.PullRequest) (remote string, trustedOrigin bool) {
	baseOwner := ""
	if pr.Repository != "" {
		baseOwner, _, _ = strings.Cut(pr.Repository, "/")
	}
	if baseOwner == "" && t.ProjectID != "" {
		baseOwner, _, _ = strings.Cut(t.ProjectID, "/")
	}
	if pr.HeadRepoOwner != "" && baseOwner != "" && strings.EqualFold(pr.HeadRepoOwner, baseOwner) {
		return "origin", true
	}
	if r.projects == nil || t.ProjectID == "" {
		return "origin", false
	}
	proj, err := r.projects.Get(t.ProjectID)
	if err != nil || proj.ClonePath == "" {
		return "origin", false
	}
	return project.PushRemote(ctx, proj.ClonePath), false
}

func (r *Handler) recoverTaskBranchConflict(ctx context.Context, t task.Task, spec taskBranchConflictRecoverySpec) bool {
	taskID := t.ID
	if r.worktreeSkip(taskID) {
		return false
	}
	if spec.branchOverride != "" && t.Branch != spec.branchOverride {
		if _, err := r.tasks.Update(taskID, task.Update{Branch: task.Ptr(spec.branchOverride)}); err != nil {
			r.logger.Warn("pr-monitor.branch-conflict.branch-override", "task_id", taskID, "branch", spec.branchOverride, "err", err)
			return false
		}
		t.Branch = spec.branchOverride
	}
	if r.prTracker.AtCap(taskID, spec.retryKind) {
		if spec.allowRecreate && r.recreateExhaustedNoPRBranch(ctx, t) {
			return true
		}
		r.markConflictRecoveryExhausted(taskID, spec.retryKind)
		return false
	}

	r.branchRecoveryMu.Lock()
	if r.branchRecoveryInFlight == nil {
		r.branchRecoveryInFlight = make(map[string]struct{})
	}
	if _, busy := r.branchRecoveryInFlight[taskID]; busy {
		r.branchRecoveryMu.Unlock()
		// Another caller is already preparing or dispatching the bounded
		// recovery for this task. Treat that as handled: callers interpret
		// false as "escalate to human-required", which would otherwise cancel
		// the recovery workflow the first caller has just started. The owner
		// of the in-flight attempt still reports its own real failure.
		return true
	}
	r.branchRecoveryInFlight[taskID] = struct{}{}
	r.branchRecoveryMu.Unlock()
	defer func() {
		r.branchRecoveryMu.Lock()
		delete(r.branchRecoveryInFlight, taskID)
		r.branchRecoveryMu.Unlock()
	}()

	proj, err := r.projects.Get(t.ProjectID)
	if err != nil {
		r.logger.Warn("pr-monitor.branch-conflict.project", "task_id", taskID, "err", err)
		return false
	}
	base, err := project.DefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		r.logger.Warn("pr-monitor.branch-conflict.base", "task_id", taskID, "err", err)
		return false
	}
	base = strings.TrimSpace(base)
	if base == "" {
		r.logger.Warn("pr-monitor.branch-conflict.base-empty", "task_id", taskID)
		return false
	}

	dir, err := r.worktrees.PrepareForBranchConflictFromRemote(ctx, t, spec.remoteOverride)
	if err != nil {
		r.logger.Warn("pr-monitor.branch-conflict.prepare", "task_id", taskID, "err", err)
		return r.parkOrEscalateBranchFixFailure(taskID, err)
	}
	if !r.allowPreparedWorktree(taskID, dir) {
		return false
	}
	if admit, handled := r.preflightPushCredentials(ctx, taskID, dir); !admit {
		return handled
	}
	// Refetch: PrepareForBranchFix's ensureBranch call may have just set
	// t.Branch for the first time (a task whose worktree never got created
	// before this recovery), and branchConflictPrompt needs the resolved name.
	if refetched, gerr := r.tasks.Get(taskID); gerr == nil {
		t = refetched
	}
	resume := r.captureBranchConflictResumeState(t)
	hadActiveWorkflow := r.WorkflowEngine.HasActiveWorkflow(taskID)

	headSHA, shaErr := project.CurrentCommit(ctx, dir)
	if shaErr != nil {
		r.logger.Warn("pr-monitor.branch-conflict.head-sha", "task_id", taskID, "err", shaErr)
	}
	trustedPushURL := ""
	if spec.trustedOrigin {
		hasGuard, guardErr := project.OriginPushHasForkOnlyGuard(ctx, dir)
		if guardErr != nil {
			r.logger.Warn("pr-monitor.branch-conflict.origin-push-guard", "task_id", taskID, "err", guardErr)
			return false
		}
		if hasGuard {
			var urlErr error
			trustedPushURL, urlErr = project.RemoteURL(ctx, dir, "origin")
			if urlErr != nil || trustedPushURL == "" {
				r.logger.Warn("pr-monitor.branch-conflict.origin-url", "task_id", taskID, "err", urlErr)
				return false
			}
		}
	}
	return r.dispatchBranchConflictRecoveryToRemote(ctx, taskID, dir, spec.prompt(ctx, t, base), t, headSHA, resume, hadActiveWorkflow, spec.retryKind, trustedPushURL)
}

func (r *Handler) recreateExhaustedNoPRBranch(ctx context.Context, t task.Task) bool {
	taskID := t.ID
	if r.worktrees == nil || r.WorkflowEngine == nil {
		return false
	}
	if r.prTracker.AtCap(taskID, branchRecreateKind) {
		return false
	}
	if err := r.worktrees.RecreateFromBase(ctx, t); err != nil {
		r.logger.Warn("pr-monitor.branch-recreate.failed", "task_id", taskID, "err", err)
		return false
	}
	if r.WorkflowEngine.HasActiveWorkflow(taskID) {
		if _, cancelErr := r.WorkflowEngine.CancelWorkflow(taskID, "branch recreated from fresh base"); cancelErr != nil {
			r.logger.Error("pr-monitor.branch-recreate.cancel", "task_id", taskID, "err", cancelErr)
			return false
		}
	}
	reason := "branch recreated from a fresh base after conflict recovery was exhausted; re-implementing (diverged commits saved under refs/sybra-backup)"
	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   taskID,
		ToStatus: task.StatusInProgress,
		Actor:    "review.pr-monitor.branch-recreate",
		Extra: task.Update{
			StatusReason: task.Ptr(reason),
		},
	}); err != nil {
		r.logger.Error("pr-monitor.branch-recreate.status", "task_id", taskID, "err", err)
		return false
	}
	r.prTracker.MarkHandled(taskID, branchRecreateKind, "")
	r.prTracker.Clear(taskID, branchConflictRetryKind)
	r.logAudit(audit.EventBranchConflictAutoResolved, taskID, "", map[string]any{"recreated": true})
	r.logger.Info("pr-monitor.branch-recreate.done", "task_id", taskID)
	return true
}

func (r *Handler) captureBranchConflictResumeState(t task.Task) branchConflictResumeState {
	state := branchConflictResumeState{
		status:       string(t.Status),
		statusReason: t.StatusReason,
		prior:        t.Workflow.Clone(),
	}
	if t.Workflow == nil {
		return state
	}
	state.workflowID = t.Workflow.WorkflowID
	state.workflowStep = t.Workflow.CurrentStep
	captured := make(map[string]string, len(t.Workflow.Variables))
	maps.Copy(captured, t.Workflow.Variables)
	if encoded, err := json.Marshal(captured); err == nil {
		state.workflowVars = string(encoded)
	} else {
		r.logger.Warn("pr-monitor.branch-conflict.resume-vars-encode", "task_id", t.ID, "err", err)
	}
	return state
}

func (r *Handler) recoverRetryablePRFixDispatch(taskID string, startErr error) bool {
	failure := workflow.ClassifyAgentStartFailure(startErr)
	if failure.Permanent {
		return false
	}
	fresh, err := r.tasks.Get(taskID)
	if err != nil {
		r.logger.Warn("pr-monitor.workflow-dispatch-retry.get", "task_id", taskID, "err", err)
		return false
	}
	if fresh.Status != task.StatusHumanRequired || fresh.Workflow == nil {
		return false
	}
	if fresh.Workflow.WorkflowID != prFixWorkflowID || fresh.Workflow.CurrentStep != "fix" {
		return false
	}
	switch fresh.Workflow.State {
	case workflow.ExecCompleted, workflow.ExecFailed:
		return false
	case workflow.ExecRunning, workflow.ExecWaiting:
	}

	update := task.Update{}
	if failure.Reason != "" {
		update.StatusReason = task.Ptr(failure.Reason)
	} else {
		update.StatusReason = task.Ptr("")
	}
	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   taskID,
		ToStatus: task.StatusInReview,
		Actor:    "review.pr-monitor.workflow-dispatch-retry",
		Extra:    update,
	}); err != nil {
		r.logger.Error("pr-monitor.workflow-dispatch-retry.status", "task_id", taskID, "err", err)
		return false
	}
	return true
}

// dispatchBranchConflictRecovery starts the branch-conflict-fix workflow,
// replacing the task's current workflow when hadActiveWorkflow is true.
//
// hadActiveWorkflow must NOT be re-derived here via HasActiveWorkflow: this
// method runs synchronously inside the very step execution of the workflow
// it may need to replace (create_pr or push_existing_pr's pushTaskBranch,
// reached from within simple-task-pr's own start/dispatch call), and using
// ReplaceWorkflow —
// rather than a separate CancelWorkflow + StartWorkflowWithVars pair — is
// what avoids a guaranteed reentrant "start in progress" failure there (see
// workflow.Engine.ReplaceWorkflow's doc).
func (r *Handler) dispatchBranchConflictRecovery(ctx context.Context, taskID, dir, prompt string, t task.Task, headSHA string, resume branchConflictResumeState, hadActiveWorkflow bool, retryKind github.PRIssueKind) bool {
	return r.dispatchBranchConflictRecoveryToRemote(ctx, taskID, dir, prompt, t, headSHA, resume, hadActiveWorkflow, retryKind, "")
}

func (r *Handler) dispatchBranchConflictRecoveryToRemote(ctx context.Context, taskID, dir, prompt string, t task.Task, headSHA string, resume branchConflictResumeState, hadActiveWorkflow bool, retryKind github.PRIssueKind, trustedPushURL string) bool {
	vars := map[string]string{
		"prompt":                prompt + PRFixResultContract,
		workflow.WorkflowVarDir: dir,
		"resume_status":         resume.status,
		"resume_status_reason":  resume.statusReason,
	}
	if trustedPushURL != "" {
		vars[workflow.WorkflowVarBranchConflictPushRemote] = "origin"
		vars[workflow.WorkflowVarBranchConflictPushURL] = trustedPushURL
	}
	if resume.workflowID != "" {
		vars["resume_workflow_id"] = resume.workflowID
		vars["resume_workflow_step"] = resume.workflowStep
		vars["resume_workflow_vars"] = resume.workflowVars
	}

	var err error
	if hadActiveWorkflow {
		err = r.WorkflowEngine.ReplaceWorkflow(taskID, "branch conflict recovery", branchConflictFixWorkflowID, vars)
	} else {
		err = r.WorkflowEngine.StartWorkflowWithVars(taskID, branchConflictFixWorkflowID, vars)
	}
	if err != nil {
		// A concurrent StartWorkflow call (e.g. restart-stale's raw
		// StartWorkflow(implement)) can grab this task's starting/dispatching
		// marker sometime during the PrepareForBranchFix/worktree-setup work
		// above — a TOCTOU window the caller's own upfront marker check
		// (TryConflictRecovery) cannot close, since that check ran before this
		// multi-second prep started. Rather than restore the prior workflow and
		// give up (previously stranding the task once the concurrent call's own
		// unaided rebase failed and escalated to human-required), queue this
		// recovery for a retry once that call releases the marker — same
		// deferred-drain contract tryConflictRecovery already gives
		// push_branch/create_pr. Do NOT restore resume.prior here: the retry
		// re-derives and re-dispatches from scratch, so restoring now would
		// just be immediately clobbered by it.
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			r.logger.Info("pr-monitor.branch-conflict.dispatch-queued", "task_id", taskID, "err", err)
			r.WorkflowEngine.QueueConflictRecoveryRetry(taskID)
			return true
		}
		r.logger.Error("pr-monitor.branch-conflict.dispatch", "task_id", taskID, "err", err)
		if resume.prior != nil {
			// startWorkflowCore's surfaceInitialDispatchFailure may have already
			// classified this same error as permanent and escalated the task to
			// human-required underneath this call (ReplaceWorkflow/StartWorkflowWithVars
			// → startWorkflowCore → surfaceStartFailureClassified). Restoring
			// resume.status/prior here would silently clobber that escalation and
			// resurrect the already-cancelled workflow. Mirror the sticky guard
			// surfaceStartFailureClassified applies: leave a task a downstream
			// handler already parked on a human alone.
			if cur, getErr := r.tasks.Get(taskID); getErr == nil && cur.Status == task.StatusHumanRequired {
				r.logger.Info("pr-monitor.branch-conflict.restore-skipped-escalated", "task_id", taskID)
			} else if _, restoreErr := r.tasks.Apply(task.TransitionIntent{
				TaskID:   taskID,
				ToStatus: task.Status(resume.status),
				Actor:    "review.pr-monitor.branch-conflict.restore",
				Extra: task.Update{
					Workflow: task.Ptr(resume.prior),
				},
			}); restoreErr != nil {
				r.logger.Error("pr-monitor.branch-conflict.restore-prior-workflow", "task_id", taskID, "err", restoreErr)
			}
		}
		if isTransientBranchConflictDispatchFailure(err) {
			return r.parkOrEscalateBranchConflictDispatchFailure(taskID, err)
		}
		return false
	}

	r.clearDispatchFailure(taskID)
	r.prTracker.MarkHandled(taskID, retryKind, headSHA)
	r.logAudit(audit.EventBranchConflictAutoResolved, taskID, "", map[string]any{})
	r.logger.Info("pr-monitor.branch-conflict.recovered", "task_id", taskID)
	return true
}

func isTransientBranchConflictDispatchFailure(err error) bool {
	var ue *provider.UnhealthyError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.RateLimited || ue.Reason == provider.RateLimitReason || !ue.Until.IsZero()
}

// dispatchFailureIsRateLimited reports whether a branch-conflict dispatch
// failure is specifically a provider rate limit (as opposed to an ambiguous
// cooldown). A rate limit always recovers, so this class parks indefinitely
// instead of consuming the escalation budget — see
// parkOrEscalateBranchConflictDispatchFailure.
func dispatchFailureIsRateLimited(err error) bool {
	var ue *provider.UnhealthyError
	if !errors.As(err, &ue) {
		return false
	}
	return ue.RateLimited || ue.Reason == provider.RateLimitReason
}

// parkOrEscalateBranchConflictDispatchFailure handles a transient
// provider-unhealthy/rate-limit error starting the branch-conflict-fix
// workflow — distinct from a genuine unresolved rebase conflict, which the
// workflow never got a chance to attempt. The caller has already restored
// the task's prior status/workflow, so the next worktree-prep rebase failure
// for this task naturally re-enters RecoverStaleBranchConflict and retries
// the dispatch; this only needs to avoid escalating straight to
// human-required on the first transient hit. Once the bounded retry count is
// spent, escalate explicitly with a reason that distinguishes it from a
// genuinely unresolved conflict — mirroring the markConflictRecoveryExhausted
// convention MarkRebaseBlocked relies on to avoid overwriting a specific
// reason with its generic one.
func (r *Handler) parkOrEscalateBranchConflictDispatchFailure(taskID string, dispatchErr error) bool {
	if dispatchFailureIsRateLimited(dispatchErr) {
		// A rate limit always recovers on its own, so park and retry each poll
		// until a provider is healthy rather than consuming the escalation
		// budget: a human cannot un-rate-limit a provider, and 5 rapid retries
		// inside a multi-minute cooldown otherwise exhaust the budget before
		// recovery (2026-07-24 kuma 5be87222). Mirrors sybra#1585's reschedule
		// park-don't-burn policy.
		r.logger.Info("pr-monitor.branch-conflict.dispatch-parked-cooldown",
			"task_id", taskID, "err", dispatchErr)
		return true
	}
	attempts := r.recordDispatchFailure(taskID)
	if attempts < branchConflictDispatchFailureLimit {
		r.logger.Info("pr-monitor.branch-conflict.dispatch-parked-retry",
			"task_id", taskID, "attempts", attempts, "limit", branchConflictDispatchFailureLimit, "err", dispatchErr)
		return true
	}
	r.clearDispatchFailure(taskID)
	reason := fmt.Sprintf(
		"branch-conflict-fix dispatch failed %d time(s), most recently: %s",
		attempts, dispatchErr.Error())
	if _, err := r.tasks.Apply(task.TransitionIntent{
		TaskID:   taskID,
		ToStatus: task.StatusHumanRequired,
		Actor:    "review.pr-monitor.branch-conflict.dispatch-exhausted",
		Extra: task.Update{
			StatusReason: task.Ptr(reason),
		},
	}); err != nil {
		r.logger.Error("pr-monitor.branch-conflict.dispatch-exhausted-status", "task_id", taskID, "err", err)
	}
	r.logger.Error("pr-monitor.branch-conflict.dispatch-exhausted", "task_id", taskID, "attempts", attempts)
	return false
}

func (r *Handler) parkOrEscalateBranchFixFailure(taskID string, wtErr error) bool {
	if r.dropTerminalWorktreeFailure(taskID, wtErr) {
		return false
	}
	attempts := r.recordWorktreeFailure(taskID, wtErr)
	if t, gerr := r.tasks.Get(taskID); gerr == nil && t.Status == task.StatusHumanRequired {
		return false
	}
	r.logger.Info("pr-monitor.branch-conflict.parked-retry",
		"task_id", taskID, "attempts", attempts, "limit", wtFailureLimit)
	return true
}

func worktreeFailureTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, worktree.ErrTaskBranchMissing) {
		return true
	}
	return strings.Contains(err.Error(), "invalid reference")
}

func (r *Handler) dropTerminalWorktreeFailure(taskID string, wtErr error) bool {
	if r.tasks == nil {
		if worktreeFailureTerminal(wtErr) {
			r.dropWorktreeEntry(taskID)
			return true
		}
		return false
	}
	got, err := r.tasks.Get(taskID)
	if errors.Is(err, os.ErrNotExist) {
		r.dropWorktreeEntry(taskID)
		r.logger.Warn("pr-monitor.worktree.task-gone", "task_id", taskID, "err", wtErr)
		return true
	}
	if err != nil {
		r.logger.Warn("pr-monitor.worktree.task-get", "task_id", taskID, "err", err)
		return false
	}
	if !worktreeFailureTerminal(wtErr) {
		return false
	}
	r.clearWorktreeFailure(taskID)
	if got.Status != task.StatusHumanRequired {
		reason := fmt.Sprintf("branch deleted: fix worktree cannot be created (%s)", wtErr)
		if _, uerr := r.tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusHumanRequired,
			Actor:    "review.pr-monitor.worktree.terminal-escalate",
			Extra: task.Update{
				StatusReason: task.Ptr(reason),
			},
		}); uerr != nil {
			r.logger.Error("pr-monitor.worktree.terminal-escalate", "task_id", taskID, "err", uerr)
		}
	}
	r.logger.Warn("pr-monitor.worktree.branch-deleted", "task_id", taskID, "err", wtErr)
	return true
}

func (r *Handler) dropWorktreeEntry(taskID string) {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	if r.wtDropped == nil {
		r.wtDropped = make(map[string]struct{})
	}
	r.wtDropped[taskID] = struct{}{}
	delete(r.wtFailures, taskID)
}

func (r *Handler) worktreeSkip(taskID string) bool {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	_, dropped := r.wtDropped[taskID]
	return dropped
}

func (r *Handler) recordDispatchFailure(taskID string) int {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	if r.dispatchFailures == nil {
		r.dispatchFailures = make(map[string]int)
	}
	r.dispatchFailures[taskID]++
	return r.dispatchFailures[taskID]
}

func (r *Handler) clearDispatchFailure(taskID string) {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	delete(r.dispatchFailures, taskID)
}

func (r *Handler) recordWorktreeFailure(taskID string, wtErr error) int {
	r.failureMu.Lock()
	if r.wtFailures == nil {
		r.wtFailures = make(map[string]int)
	}
	r.wtFailures[taskID]++
	attempts := r.wtFailures[taskID]
	if attempts >= wtFailureLimit {
		delete(r.wtFailures, taskID)
		r.failureMu.Unlock()
		r.logger.Error("pr-monitor.worktree.circuit-open",
			"task_id", taskID, "failures", wtFailureLimit, "err", wtErr)
		if _, uerr := r.tasks.Apply(task.TransitionIntent{
			TaskID:   taskID,
			ToStatus: task.StatusHumanRequired,
			Actor:    "review.pr-monitor.worktree.escalate",
			Extra: task.Update{
				StatusReason: task.Ptr(fmt.Sprintf("pr-monitor: worktree creation failed %d times", wtFailureLimit)),
			},
		}); uerr != nil {
			r.logger.Error("pr-monitor.worktree.escalate", "task_id", taskID, "err", uerr)
		}
		return attempts
	}
	r.failureMu.Unlock()
	r.logger.Error("pr-monitor.worktree", "task_id", taskID, "err", wtErr)
	return attempts
}

func (r *Handler) clearWorktreeFailure(taskID string) {
	r.failureMu.Lock()
	defer r.failureMu.Unlock()
	delete(r.wtFailures, taskID)
}

func (r *Handler) allowPreparedWorktree(taskID, dir string) bool {
	setupFailure, ok, err := worktree.ReadSetupFailureMarker(dir)
	if err != nil {
		r.logger.Error("pr-monitor.worktree.setup-fail-read", "task_id", taskID, "dir", dir, "err", err)
		r.recordWorktreeFailure(taskID, err)
		return false
	}
	if !ok {
		r.clearWorktreeFailure(taskID)
		return true
	}
	if setupFailure == "" {
		setupFailure = "worktree setup failed before the fix agent started"
	}
	err = errors.New(setupFailure)
	r.logger.Warn("pr-monitor.worktree.setup-fail-nonfatal", "task_id", taskID, "dir", dir, "err", err)
	r.recordWorktreeFailure(taskID, err)
	got, err := r.tasks.Get(taskID)
	if err != nil {
		r.logger.Error("pr-monitor.worktree.setup-fail-refetch", "task_id", taskID, "err", err)
		return false
	}
	return got.Status != task.StatusHumanRequired
}

// branchConflictPrompt is the no-PR analog of buildConflictPrompt: there is no
// PR head to reference, so it resolves the task's own branch (t.Branch, set
// by PrepareForBranchFix before this is called). Unlike the PR-backed conflict
// prompt, this path may allow a rebase plus force-with-lease because no PR
// exists yet.
func branchConflictPrompt(ctx context.Context, t task.Task, base string) string {
	branch := t.Branch
	if branch == "" {
		branch = "the task's current branch"
	}
	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (this task has no PR yet). "+
			"Use the task description and current code as context, then merge.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git merge refs/remotes/origin/%s\n"+
			"# If the merge stopped for conflicts: resolve every conflict preserving\n"+
			"# this branch's intent and the upstream changes, then git add and git\n"+
			"# commit %s to complete the merge.\n"+
			"# If the merge already completed on its own (clean/fast-forward, no\n"+
			"# conflicts), it is already committed — do not run git commit again, it\n"+
			"# will fail with \"nothing to commit\".\n"+
			"# If a merge becomes too tangled, you may instead rebase onto\n"+
			"# refs/remotes/origin/%s, resolve the conflicts there, and finish with a\n"+
			"# lease-protected force-push because no PR exists yet.\n"+
			"%s\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `refs/remotes/origin/%s` (not `origin/%s`) to avoid ambiguous refs\n"+
			"- Push to `fork` (not `origin`) when a `fork` remote exists — the branch was opened from the fork\n"+
			"- Prefer a merge; if you must rebase before the first PR exists, push the rewritten branch back with `--force-with-lease`\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- Do not stop just because the conflict count is high — split by file and resolve all conflicts autonomously\n"+
			"- Before pushing, run tests for touched code as a single blocking foreground command (e.g. `go test ./pkg/foo/...`) and wait for it to exit — never background a test run or narrate/poll its progress; if it has not finished within a couple of minutes, stop and report a blocker instead of waiting indefinitely\n"+
			"- Stop only for a concrete blocker: binary conflict, missing secret/credential, deleted context you cannot reconstruct, or a semantic decision that the task context does not answer\n"+
			"- No investigation, no extra commits, no unrelated changes",
		branch, base, project.CommitSignFlags(ctx), base, prFixPushPrompt(branch, "", false, true), base, base,
	)
}

func sameBranchConflictPrompt(ctx context.Context, t task.Task, remote string) string {
	branch := t.Branch
	if branch == "" {
		branch = "the task's current branch"
	}
	if remote == "" {
		remote = "origin"
	}
	prCtx := ""
	if t.PRNumber > 0 {
		prCtx = fmt.Sprintf(" backing PR #%d", t.PRNumber)
	}
	return fmt.Sprintf(
		"Resolve the content conflict between the LOCAL copy of branch `%s` and the already-pushed REMOTE copy of that SAME branch%s.\n"+
			"This is not a base-branch rebase conflict; preserve both lines of work with an additive merge.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch %s +refs/heads/%s:refs/remotes/%s/%s\n"+
			"git merge refs/remotes/%s/%s\n"+
			"# If the merge stopped for conflicts: resolve every conflict preserving\n"+
			"# both the local follow-up commits and the already-pushed remote commits,\n"+
			"# then git add and git commit %s to finish the merge.\n"+
			"# If the merge already completed on its own (clean/no-op), do not run git\n"+
			"# commit again.\n"+
			"# Do NOT rebase, amend, or force-push: this branch already backs a live PR.\n"+
			"```\n\n"+
			"Do not push: Sybra will verify and publish this completed merge through its trusted, non-force workflow step. Summarize what conflicted and how you resolved it.",
		branch, prCtx, remote, branch, remote, branch, remote, branch, project.CommitSignFlags(ctx),
	)
}

// prepareWorktree sets up the fix worktree for the given task and PR issue.
// Returns ("", false) on error, with circuit-breaker escalation after wtFailureLimit
// consecutive failures. Returns ("", true) when no worktree is needed.
func (r *Handler) prepareWorktree(ctx context.Context, t task.Task, issue github.PRIssue) (string, bool) {
	if t.ProjectID == "" {
		return "", true
	}
	if r.worktreeSkip(t.ID) {
		return "", false
	}
	var (
		d     string
		wtErr error
	)
	// Conflict and comment fixes operate on the PR's existing branch, so check
	// it out (PrepareForFix). A CI fix normally re-runs on a fresh worktree, but
	// approved PRs must also use the branch-preserving fix path: PrepareForTask
	// rebases onto base and can fall back to an additive merge before the agent
	// starts, which changes the merge-base and can dismiss an existing approval
	// even when the agent would make no substantive fix.
	if issue.Kind == github.PRIssueConflict || issue.Kind == github.PRIssueComments || prHasCurrentApproval(issue.PR) {
		d, wtErr = r.worktrees.PrepareForFix(ctx, t, issue.PR.Number)
	} else {
		d, wtErr = r.worktrees.PrepareForTask(ctx, t, nil)
	}
	if wtErr != nil {
		if errors.Is(wtErr, worktree.ErrAgentRunning) {
			r.logger.Warn("pr-monitor.worktree.agent-running", "task_id", t.ID, "err", wtErr)
			return "", false
		}
		if errors.Is(wtErr, project.ErrBranchDiverged) {
			remote, trustedOrigin := r.sameBranchConflictRemote(ctx, t, issue.PR)
			if r.recoverSameBranchConflict(ctx, t, issue.PR.HeadRefName, remote, trustedOrigin) {
				return "", false
			}
		}
		// A conflict fix already operates on the non-rebasing PrepareForFix path,
		// so a rebase failure here can only come from the CI-fix PrepareForTask
		// branch. Recover by re-routing to the conflict fix unless this already
		// IS a conflict fix (avoid re-entering ourselves).
		var recoverFn func(string) bool
		if issue.Kind != github.PRIssueConflict {
			recoverFn = r.RecoverStaleBranchConflict
		}
		if agentorch.MarkRebaseBlocked(r.tasks, t.ID, wtErr, r.logger, recoverFn) {
			return "", false
		}
		if r.dropTerminalWorktreeFailure(t.ID, wtErr) {
			return "", false
		}
		r.recordWorktreeFailure(t.ID, wtErr)
		return "", false
	}
	if !r.allowPreparedWorktree(t.ID, d) {
		return "", false
	}
	return d, true
}

// commentsPrompt instructs the fix agent to address unresolved review comments
// on the user's own PR via the /fix-review skill (which replies on every
// thread), then push and re-request review.
func commentsPrompt(ctx context.Context, pr github.PullRequest) string {
	return fmt.Sprintf(
		"Run /fix-review %s --auto\n\n"+
			"This is your own PR (#%d) — reviewers left comments or unresolved "+
			"threads. Address the valid ones, reply on every thread, and push.\n\n"+
			"End every thread reply and any PR comment you post with a blank line "+
			"then the harness attribution footer, exactly: `_Generated by Sybra "+
			"harness_`.\n\n"+
			"Before posting each thread reply, re-fetch that thread and skip it "+
			"if the authenticated user already posted a reply containing the "+
			"harness footer on the current PR head. Never post a reply body "+
			"containing placeholders like `__SHA__`, `<short-sha>`, or "+
			"`TODO`; replace them before the first API call. If a reply API "+
			"call succeeds for a thread, do not retry the same thread through "+
			"another API path.\n\n"+
			"Never weaken, skip, delete, or hardcode tests to satisfy a comment "+
			"— fix the underlying code; tampering is detected and blocks the "+
			"task.\n\n"+
			"IMPORTANT: when committing, use conventional commit format "+
			"`fix(review): address PR review comments` (type(scope) required by "+
			"repo hooks). Sign the commit with `git commit %s`.\n\n"+
			"%s",
		pr.URL, pr.Number, project.CommitSignFlags(ctx), prFixPushPrompt(pr.HeadRefName, "Push to the same remote create-pr would target for this worktree:", true, false),
	)
}

func conflictPrompt(ctx context.Context, pr github.PullRequest) string {
	var filesCtx string
	if files, err := github.FetchPRFiles(pr.Repository, pr.Number); err == nil && len(files) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\nFiles changed in this PR:\n")
		for _, f := range files {
			sb.WriteString("- ")
			sb.WriteString(f)
			sb.WriteByte('\n')
		}
		filesCtx = sb.String()
	}

	return buildConflictPrompt(ctx, pr, filesCtx)
}

func buildConflictPrompt(ctx context.Context, pr github.PullRequest, filesCtx string) string {
	base := pr.BaseRefName
	if base == "" {
		base = "main"
	}
	baseRef := "refs/remotes/origin/" + base
	return fmt.Sprintf(
		"Fix merge conflicts on branch `%s` (PR #%d). "+
			"Use the task body, PR diff, changed-file list, and current code as context, then merge.\n\n"+
			"Steps:\n"+
			"```bash\n"+
			"git fetch origin\n"+
			"git merge %s\n"+
			"# If the merge stopped for conflicts: resolve every conflict preserving\n"+
			"# the PR intent and upstream changes, then git add and git commit %s to\n"+
			"# complete the merge.\n"+
			"# If the merge already completed on its own (clean/fast-forward, no\n"+
			"# conflicts), it is already committed — do not run git commit again, it\n"+
			"# will fail with \"nothing to commit\".\n"+
			"%s\n"+
			"```\n\n"+
			"Rules:\n"+
			"- Use `%s` (the PR's base branch, a full ref not a bare name) to avoid ambiguous refs\n"+
			"- Push to `fork` (not `origin`) when a `fork` remote exists — the PR was opened from the fork\n"+
			"- Do not force-push or rewrite existing commits — the merge commit and any conflict-resolution commits must be purely additive, and a plain push is expected to succeed\n"+
			"- Resolve conflicts keeping BOTH sides' intent\n"+
			"- Do not stop just because the conflict count is high — split by file and resolve all conflicts autonomously\n"+
			"- Before pushing, run tests for touched code as a single blocking foreground command (e.g. `go test ./pkg/foo/...`) and wait for it to exit — never background a test run or narrate/poll its progress; if it has not finished within a couple of minutes, stop and report a blocker instead of waiting indefinitely\n"+
			"- Stop only for a concrete blocker: binary conflict, missing secret/credential, deleted context you cannot reconstruct, or a semantic decision that the task/PR context does not answer\n"+
			"- No investigation, no extra commits, no unrelated changes"+
			"%s",
		pr.HeadRefName, pr.Number, baseRef, project.CommitSignFlags(ctx), prFixPushPrompt(pr.HeadRefName, "", false, false), baseRef, filesCtx,
	)
}
