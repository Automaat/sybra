package workflow

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/evidence"
)

// Criterion names used across the deterministic gates and the
// require_evidence step. Kept as named constants (not the step IDs directly)
// so a step can be renamed in a workflow YAML without silently breaking the
// evidence it records/consults.
const (
	evidenceCriterionVerifyCommits   = "verify_commits"
	evidenceCriterionCodegenGate     = "codegen_gate"
	evidenceCriterionFocusedChecks   = "focused_checks"
	evidenceCriterionDetectTampering = "detect_tampering"
	evidenceCriterionVerifyChecks    = "verify_checks"
	evidenceCriterionTestRunner      = "test_runner"
	evidenceCriterionReview          = "review"
)

// evidenceGateReasonPrefix marks a human-required status_reason as written by
// require_evidence, mirroring TamperFlaggedReasonPrefix/
// admissionPreflightReasonPrefix's pattern for other mechanical gates.
const evidenceGateReasonPrefix = "completion evidence incomplete:"

// freshnessExemptCriteria names criteria whose proof asserts a monotonic
// historical fact rather than a property of the current tree, so a commit
// landing after they ran cannot invalidate them. Only verify_commits
// qualifies: it proves "the agent produced commits ahead of origin/main", and
// the two commit-producing steps that routinely run after it — the codegen
// gate's checkpoint commit (implement stage) and the review-fix commits
// (review stage) — only ever add commits, never unmake that fact. Without this
// exemption the require_evidence gate would flag verify_commits stale on the
// ordinary happy path (any project with checks.codegen configured, or any task
// that took one review-fix round). Every content-validating criterion
// (detect_tampering, verify_checks, test_runner, review) stays freshness-gated,
// since a later commit genuinely can change what they asserted.
var freshnessExemptCriteria = map[string]bool{
	evidenceCriterionVerifyCommits: true,
}

// recordEvidence best-effort appends one CriterionEvidence entry for taskID.
// A missing recorder, worktree getter, or worktree is a silent no-op — this
// is instrumentation, not a gate, and must never alter the caller's own
// pass/fail outcome or timing. resultDigest is hashed here (not by the
// caller) so producers can pass raw report/output text without duplicating
// evidence.Digest at every call site.
func (e *Engine) recordEvidence(taskID, stepID, criterion string, proofType evidence.ProofType, exitStatus int, command, result string) {
	if e.evidenceRecorder == nil || e.worktrees == nil {
		return
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	entry := evidence.CriterionEvidence{
		Criterion:    criterion,
		ProofType:    proofType,
		Command:      command,
		ExitStatus:   exitStatus,
		ResultDigest: evidence.Digest(result),
		BaseRev:      resolveOriginBase(ctx, wtPath),
		FinalRev:     revParseCommit(ctx, wtPath, "HEAD"),
		Backend:      evidenceBackendIdentity(),
		StepID:       stepID,
		Timestamp:    time.Now().UTC(),
	}
	if err := e.evidenceRecorder.AppendCriterion(taskID, entry); err != nil {
		e.logger.Warn("workflow.evidence.append-failed",
			"task_id", taskID, "criterion", criterion, "err", err)
	}
}

// evidenceBackendIdentity best-effort identifies the machine that produced a
// proof. Empty on a lookup failure — an informational field, not part of the
// require_evidence pass/fail decision.
func evidenceBackendIdentity() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// requiredEvidenceCriteria returns the criteria require_evidence demands
// fresh, passing evidence for — scoped to what is actually applicable to this
// task, mirroring each gate's own skip conditions, so a project with no
// configured verify suite (or a task that never went through review) is never
// blocked on a criterion that could never have produced evidence in the first
// place.
func (e *Engine) requiredEvidenceCriteria(taskID string, wfExec *Execution, t TaskInfo) []string {
	var required []string
	if e.worktrees != nil {
		if _, ok := e.worktrees.GetWorktreePath(taskID); ok {
			required = append(required, evidenceCriterionVerifyCommits, evidenceCriterionDetectTampering)
		}
	}
	if e.checks != nil && len(e.checks.VerifyCommands(e.ctx, taskID)) > 0 {
		required = append(required, evidenceCriterionVerifyChecks)
	}
	if wfExec != nil && wfExec.CountStep(testVerdictSourceStep) > 0 {
		required = append(required, evidenceCriterionTestRunner)
	}
	if t.Reviewed {
		required = append(required, evidenceCriterionReview)
	}
	return required
}

// execRequireEvidence is the final deterministic completion gate: every
// criterion requiredEvidenceCriteria names for this task must have a
// CompletionEvidence entry that passed (ExitStatus 0) and, unless the
// criterion is in freshnessExemptCriteria, is fresh (FinalRev equals the
// task's current HEAD) — otherwise the task flips to human-required with a
// terminal blocker.KindOperatorDecision instead of landing on stale or
// missing proof.
//
// No-op (completed, no block) when:
//   - the gate is disabled (config.EvidenceConfig.Enabled=false, the default)
//     or no EvidenceRecorder is wired
//   - the task has no recorded CompletionEvidence at all — an in-flight task
//     from before evidence collection started; retroactively blocking it
//     would strand work no producer ever had a chance to instrument
//   - the worktree/HEAD cannot be resolved — verify_commits and
//     detect_tampering already gate a broken worktree upstream, so this is
//     not require_evidence's failure to report
func (e *Engine) execRequireEvidence(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if !e.evidence.Enabled || e.evidenceRecorder == nil {
		return stepDone(step, "skipped: evidence gate disabled")
	}
	ce, err := e.evidenceRecorder.Evidence(taskID)
	if err != nil {
		e.logger.Warn("workflow.require-evidence.load-failed", "task_id", taskID, "err", err)
		return stepDone(step, "skipped: evidence unreadable")
	}
	if len(ce.Criteria) == 0 {
		return stepDone(step, "skipped: no evidence baseline recorded")
	}

	headSHA := e.currentHeadSHA(taskID)
	if headSHA == "" {
		return stepDone(step, "skipped: no worktree or HEAD unresolved")
	}

	var problems []string
	for _, criterion := range e.requiredEvidenceCriteria(taskID, wfExec, t) {
		entry, ok := ce.ByCriterion(criterion)
		switch {
		case !ok:
			problems = append(problems, criterion+": missing")
		case !entry.Passed():
			problems = append(problems, fmt.Sprintf("%s: failed (exit %d)", criterion, entry.ExitStatus))
		case !freshnessExemptCriteria[criterion] && entry.FinalRev != headSHA:
			problems = append(problems, fmt.Sprintf("%s: stale (recorded at %s, HEAD is %s)",
				criterion, trimDiffLine(entry.FinalRev), trimDiffLine(headSHA)))
		}
	}

	if len(problems) > 0 {
		reason := evidenceGateReasonPrefix + " " + strings.Join(problems, "; ")
		if err := e.tasks.UpdateTaskBlocker(taskID, "human-required", reason, blocker.State{
			Kind:      blocker.KindOperatorDecision,
			Actor:     blocker.ActorWorkflow,
			Exhausted: true,
		}); err != nil {
			return StepOutput{}, fmt.Errorf("require-evidence: set human-required: %w", err)
		}
		e.fireEvidenceDecision(t, EvidenceDecision{Outcome: "blocked", Reason: reason})
		e.logger.Warn("workflow.require-evidence.blocked", "task_id", taskID, "problems", problems)
		return stepDone(step, "blocked: "+reason)
	}

	e.fireEvidenceDecision(t, EvidenceDecision{Outcome: "verified"})
	e.logger.Info("workflow.require-evidence.verified", "task_id", taskID, "head", headSHA)
	return stepDone(step, "complete")
}

func (e *Engine) fireEvidenceDecision(t TaskInfo, d EvidenceDecision) {
	if e.evidenceHook == nil {
		return
	}
	e.evidenceHook(t, d)
}

// currentHeadSHA resolves the task worktree's current HEAD commit. Empty when
// no WorktreeGetter is wired or the task has no worktree.
func (e *Engine) currentHeadSHA(taskID string) string {
	if e.worktrees == nil {
		return ""
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	return revParseCommit(ctx, wtPath, "HEAD")
}
