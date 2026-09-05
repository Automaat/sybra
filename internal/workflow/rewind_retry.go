package workflow

import (
	"slices"
	"strconv"
	"time"
)

// rewindRetryPolicy is the shape shared by a bounded retry that re-arms by
// rewinding the workflow to a *different* step, rather than re-arming the
// current one in place the way boundedRetryPolicy does. Testing auto-retry
// (route_test_result re-dispatching run_test) and verify-checks auto-fix
// (rewinding a failed verify run back to implement) both: read a per-key
// attempt counter, and either bump it — stashing a backoff + reask note,
// clearing the rewind target's step history, and moving CurrentStep to it —
// or leave the counter untouched once the cap is spent so the caller can run
// its own escalation. Each caller still owns cap-exhaustion entirely
// (testing offers a human-required or a ready-pr escalation depending on the
// failure kind; verify-checks always flags for human review) — rewindRetry
// only unifies the counter read/increment/cap-compare and rewind mechanics.
type rewindRetryPolicy struct {
	// counterKey is the workflow variable holding this policy's attempt count.
	counterKey string
	max        int
	// rewindStep is the step ID CurrentStep rewinds to and whose step-history
	// records are cleared so its own max_retries budget isn't seen as
	// already spent from an earlier cycle.
	rewindStep string
	// backoff computes the retry-after delay from the pre-increment attempt
	// count (verify-checks grows it per attempt; testing uses a constant).
	backoff func(attempts int) time.Duration
	// fingerprint identifies the failure being retried. When set, repeated
	// identical failures can exhaust earlier than the generic attempt ceiling.
	fingerprint            string
	maxSameFingerprintRuns int
	// attemptProducedWork reports whether the run this policy is retrying
	// actually changed anything. The same-fingerprint cap exists to stop a
	// repair loop that makes no progress, but it could not tell "the agent
	// tried and failed" from "the agent never touched the file" — a run that
	// aborted, or ended without committing, spent the budget just the same.
	// Nil keeps the occurrence charged unconditionally.
	attemptProducedWork func(TaskInfo) bool
	// onArm sets any additional workflow variables the rewound step's prompt
	// needs (reask note, cleared verdict vars) once the counter has been
	// bumped to `attempt`, before the workflow is persisted. Optional.
	onArm func(wfExec *Execution, attempt int)
	// reason builds the task status-reason recorded alongside the rewind.
	reason func(attempt int) string
}

// lastAuthorRunProducedWork reports whether the most recent code-author run on
// the task committed anything.
//
// HeadSHA and FinalCommitSource are recorded only once a run's commit is
// observed, so their absence on a code-author run is the same evidence
// verify_commits acts on: nothing was produced. Verifier roles are ignored —
// producing no commit is what they are for.
func lastAuthorRunProducedWork(t TaskInfo) bool {
	// Index rather than value-range: AgentRunInfo is large enough that a
	// per-iteration copy is flagged.
	for i := range slices.Backward(t.AgentRuns) {
		if !isCodeAuthorRun(t.AgentRuns[i]) {
			continue
		}
		return t.AgentRuns[i].HeadSHA != "" || t.AgentRuns[i].FinalCommitSource != ""
	}
	// No code-author run recorded at all: charge the occurrence rather than
	// looping, since there is no evidence either way.
	return true
}

// rewindRetry applies a rewindRetryPolicy. armed=true means the counter was
// bumped and the workflow rewound to p.rewindStep — the caller must treat
// this tick as parked (rewindRetry does not return a step-parked sentinel
// itself since the two callers use different ones). armed=false means the
// cap was already spent and rewindRetry made no changes; the caller owns
// escalation. err!=nil can only accompany armed=true: the counter bump
// itself succeeded but persisting it or updating task status failed, so the
// caller must propagate the error rather than treat this as "go escalate".
func (e *Engine) rewindRetry(taskID string, wfExec *Execution, t TaskInfo, p rewindRetryPolicy) (armed bool, attempt int, err error) {
	attempts := parseWorkflowInt(wfExec.Variables[p.counterKey])
	if attempts >= p.max {
		return false, attempts, nil
	}
	if p.fingerprint != "" && p.maxSameFingerprintRuns > 0 {
		fpKey := p.counterKey + ".fingerprint"
		fpCountKey := p.counterKey + ".fingerprint_count"
		if wfExec.Variables[fpKey] == p.fingerprint && parseWorkflowInt(wfExec.Variables[fpCountKey]) >= p.maxSameFingerprintRuns {
			if p.attemptProducedWork == nil || p.attemptProducedWork(t) {
				return false, attempts, nil
			}
			// The previous run left no commit, so it was not an attempt at
			// this failure. Give the occurrence back rather than escalating on
			// work that never happened. The generic attempt ceiling above
			// still bounds the loop, so a run that never commits cannot spin
			// here forever.
			wfExec.SetVar(fpCountKey, strconv.Itoa(parseWorkflowInt(wfExec.Variables[fpCountKey])-1))
		}
	}

	attempt = attempts + 1
	wfExec.SetVar(p.counterKey, strconv.Itoa(attempt))
	if p.fingerprint != "" {
		fpKey := p.counterKey + ".fingerprint"
		fpCountKey := p.counterKey + ".fingerprint_count"
		count := 1
		if wfExec.Variables[fpKey] == p.fingerprint {
			count = parseWorkflowInt(wfExec.Variables[fpCountKey]) + 1
		}
		wfExec.SetVar(fpKey, p.fingerprint)
		wfExec.SetVar(fpCountKey, strconv.Itoa(count))
	}
	wfExec.SetVar(workflowRetryAfterVar, e.now().Add(p.backoff(attempts)).Format(time.RFC3339))
	if p.onArm != nil {
		p.onArm(wfExec, attempt)
	}
	wfExec.ClearStepRecords(p.rewindStep)
	wfExec.CurrentStep = p.rewindStep
	wfExec.State = ExecWaiting

	if err := e.tasks.SetStatusAndWorkflow(taskID, string(t.Status), p.reason(attempt), wfExec); err != nil {
		return true, attempt, err
	}
	return true, attempt, nil
}
