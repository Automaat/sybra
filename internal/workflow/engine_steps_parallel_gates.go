package workflow

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/evidence"
)

// gateTamperStepID, gateFocusedStepID, and gateVerifyStepID are the synthetic
// child step IDs execParallelGates uses when invoking each gate's own
// exec/compute functions — the literal IDs the three steps carried when they
// ran serially. Evidence is keyed by a fixed criterion constant, not by this
// ID (see recordEvidence), so reusing the historic names only keeps
// artifacts/logs/evidence.StepID readable; it changes no routing behavior.
const (
	gateTamperStepID  = "detect_tampering"
	gateFocusedStepID = "focused_checks"
	gateVerifyStepID  = "verify_checks"
)

// execParallelGates runs the three deterministic post-implement gates —
// detect_tampering, focused_checks, verify_checks — concurrently instead of
// serially, then joins before routing. Every gate still records its own
// evidence/status exactly as it did when the three ran as separate steps;
// this only overlaps their wall-clock.
//
// detect_tampering never touches wfExec, so it runs fully compute+apply in
// its own goroutine. focused_checks and verify_checks each keep a "compute"
// phase (run commands, classify — safe to run concurrently) split from an
// "apply" phase (task status, evidence, and — on an auto-fixable failure — a
// rewind back to implement, which mutates the shared *Execution and so must
// not run on more than one goroutine at a time); their computes run
// concurrently here, and their applies run serially afterward on this
// goroutine — see resolveParallelGates for the precedence.
//
// verify_checks' single-slot backpressure semaphore and node/toolchain
// repair are resolved once, up front, before any gate goroutine starts: this
// avoids parking after the other two gates already did wasted work, and
// avoids racing a concurrent focused_checks command run against verify's own
// `npm ci` repair mutating node_modules underneath it.
func (e *Engine) execParallelGates(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	verifyPre, parked, err := e.preflightVerifyChecks(taskID, step, wfExec, t)
	if parked != nil {
		return *parked, err
	}
	if verifyPre.releaseSlot != nil {
		defer verifyPre.releaseSlot()
	}

	tamperStep := &Step{ID: gateTamperStepID}
	focusedStep := &Step{ID: gateFocusedStepID}
	verifyStep := &Step{ID: gateVerifyStepID}

	var (
		tamperOut      StepOutput
		tamperErr      error
		focusedVerdict focusedChecksVerdict
		verifyVerdict  verifyChecksVerdict
	)

	var wg sync.WaitGroup
	wg.Go(func() {
		tamperOut, tamperErr = e.execDetectTampering(taskID, tamperStep, t)
	})
	wg.Go(func() {
		focusedVerdict = e.computeFocusedChecksVerdict(taskID, focusedStep, t)
	})
	if verifyPre.needsRun {
		wg.Go(func() {
			verifyVerdict = e.computeVerifyChecksVerdict(taskID, verifyStep, verifyPre.wtPath, verifyPre.cmds, verifyPre.timeout)
			verifyVerdict.treeSHA, verifyVerdict.checksHash = verifyPre.treeSHA, verifyPre.checksHash
		})
	}
	wg.Wait()

	if tamperErr != nil {
		return StepOutput{}, tamperErr
	}
	if focusedVerdict.err != nil {
		return StepOutput{}, focusedVerdict.err
	}

	return e.resolveParallelGates(taskID, step, wfExec, t, tamperOut, focusedStep, focusedVerdict, verifyStep, verifyPre, verifyVerdict)
}

// verifyChecksPreflight is the result of resolving verify_checks' short
// circuits (blessed tag, unconfigured, cache hit, unresolved node_modules
// corruption) and — only when the suite actually needs to run — acquiring
// its concurrency slot and running node/toolchain repair, all before any
// gate goroutine starts.
type verifyChecksPreflight struct {
	needsRun            bool
	wtPath              string
	cmds                []string
	timeout             time.Duration
	treeSHA, checksHash string
	releaseSlot         func()
	// precomputed holds the final StepOutput when verify_checks resolved
	// without needing to run the suite at all.
	precomputed *StepOutput
}

// preflightVerifyChecks resolves everything about verify_checks that must
// happen before fan-out. A non-nil parked return means the whole coordinator
// step is already done (a backpressure park) and the caller must return
// immediately without starting any gate goroutine.
func (e *Engine) preflightVerifyChecks(taskID string, step *Step, wfExec *Execution, t TaskInfo) (pre verifyChecksPreflight, parked *StepOutput, err error) {
	verifyStep := &Step{ID: gateVerifyStepID}

	if slices.Contains(t.Tags, verifyBlessedTag) {
		e.logger.Info("workflow.verify-checks.blessed", "task_id", taskID)
		e.recordEvidence(taskID, verifyStep.ID, evidenceCriterionVerifyChecks, evidence.ProofManual,
			0, "human bless ("+verifyBlessedTag+" tag)", "blessed")
		out, _ := stepDone(verifyStep, "blessed")
		return verifyChecksPreflight{precomputed: &out}, nil, nil
	}

	cmds, wtPath, timeout, skip := e.loadVerifyChecksInputs(taskID)
	if skip != "" {
		out, _ := stepDone(verifyStep, skip)
		return verifyChecksPreflight{precomputed: &out}, nil, nil
	}

	treeSHA, checksHash := e.verifyChecksCacheKey(e.ctx, wtPath, cmds)
	if treeSHA != "" {
		if out, hit := e.verifyChecksCacheHit(taskID, verifyStep, wtPath, treeSHA, checksHash); hit {
			return verifyChecksPreflight{precomputed: &out}, nil, nil
		}
	}

	slot := e.verifyChecksSlot()
	release, ok := e.acquireVerifyChecksSlot(slot)
	if !ok {
		if wfExec == nil {
			select {
			case slot <- struct{}{}:
				release = func() { <-slot }
			case <-e.ctx.Done():
				e.logger.Warn("workflow.verify-checks.canceled", "task_id", taskID, "err", e.ctx.Err())
				out, _ := stepDone(verifyStep, "skipped: context canceled")
				return verifyChecksPreflight{precomputed: &out}, nil, nil
			}
		} else {
			// Park the coordinator itself (not the individual verify_checks
			// step, which no longer exists on its own in the YAML) so resume
			// re-enters execParallelGates and re-runs every gate — cheap,
			// since a parked run never got past this preflight.
			out, perr := e.parkVerifyChecksForBackpressure(taskID, step, wfExec, t)
			return verifyChecksPreflight{}, &out, perr
		}
	}

	if e.repairCorruptedNodeModules(e.ctx, taskID, wtPath) {
		e.logger.Warn("workflow.verify-checks.node-modules-repair-unresolved", "task_id", taskID)
		out, ferr := e.flagVerifyChecks(taskID, verifyStep,
			"verify suite could not repair corrupted node_modules before running checks — rerun setup or fix the toolchain state",
			"node_modules-repair")
		release()
		return verifyChecksPreflight{precomputed: &out}, nil, ferr
	}
	e.repairTornNodeModules(e.ctx, taskID, wtPath)

	return verifyChecksPreflight{
		needsRun: true, wtPath: wtPath, cmds: cmds, timeout: timeout,
		treeSHA: treeSHA, checksHash: checksHash, releaseSlot: release,
	}, nil, nil
}

// resolveParallelGates applies the joined gate results in a fixed
// precedence, mirroring what the three-step serial chain would have landed
// on if every failure had happened simultaneously:
//
//  1. detect_tampering flagged a high-severity finding -> human-required.
//     It already applied its own status write; the other two gates' work is
//     discarded (harmless — their computes wrote artifacts/report files
//     only, no task status).
//  2. verify_checks resolved to blocked (a classified non-auto-fixable
//     failure) -> blocked, regardless of focused_checks' outcome.
//  3. verify_checks resolved to human-required without ever running the
//     suite concurrently (blessed/skip/cache short circuits do not apply
//     here) or via a terminal flag (timeout/setup/auto-fix-ceiling) ->
//     human-required; focused_checks' outcome is folded in below only when
//     verify_checks actually ran the suite this round.
//  4. Either gate hit an auto-fixable failure -> rewind to implement. Both
//     gates' applies run, so a simultaneous double failure carries both reask
//     notes and bumps both counters — the "single merged rewind" the plan
//     calls for.
//  5. Both clean -> advance (task.status unchanged; the YAML's pr_number
//     check decides ready-review vs in-review, same as before).
//
// A terminal gate result and a rewind can never both be applied in one round.
// rewindRetry re-writes the STALE pre-gate task status, so a rewind applied
// after a gate escalated would silently erase that escalation and resume
// implementation — the serial chain could not do this, because it never ran a
// later gate once an earlier one escalated. Terminal verdicts are therefore
// classified before any apply (see applyTerminalGates), and the ordering below
// early-exits as soon as an apply turns out terminal.
func (e *Engine) resolveParallelGates(
	taskID string, step *Step, wfExec *Execution, t TaskInfo,
	tamperOut StepOutput,
	focusedStep *Step, focusedVerdict focusedChecksVerdict,
	verifyStep *Step, verifyPre verifyChecksPreflight, verifyVerdict verifyChecksVerdict,
) (StepOutput, error) {
	if tamperOut.Output == "flagged" {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "human-required: detect_tampering flagged"}, nil
	}

	if verifyPre.precomputed != nil {
		vOut := *verifyPre.precomputed
		if vOut.Output == "flagged" {
			return StepOutput{StepID: step.ID, Status: "completed", Output: "human-required: verify_checks flagged"}, nil
		}
		focusedOut, focusedErr := e.applyFocusedChecksVerdict(taskID, focusedStep, wfExec, t, focusedVerdict)
		if focusedErr != nil {
			if errors.Is(focusedErr, errStepParked) {
				return StepOutput{}, errStepParked
			}
			return StepOutput{}, focusedErr
		}
		if focusedOut.Output == "flagged" {
			return StepOutput{StepID: step.ID, Status: "completed", Output: "human-required: focused_checks flagged"}, nil
		}
		return StepOutput{StepID: step.ID, Status: "completed", Output: "clean"}, nil
	}

	// Resolve terminal verdicts (a human-required/blocked status write that
	// never rewinds) before applying anything: only the terminal gate(s) then
	// apply, so no rewind can overwrite the escalation with the stale pre-gate
	// status.
	focusedTerminal := e.focusedVerdictTerminal(focusedVerdict)
	verifyTerminal := e.verifyVerdictTerminal(verifyVerdict)
	if focusedTerminal || verifyTerminal {
		return e.applyTerminalGates(taskID, step, wfExec, t,
			focusedStep, focusedVerdict, focusedTerminal,
			verifyStep, verifyVerdict, verifyTerminal)
	}

	// Neither gate is terminal by verdict, so each apply is a no-op
	// (clean/skip) or an auto-fix rewind — except that a rewind whose ceiling
	// is already reached escalates instead. Apply the gate with MORE prior
	// auto-fix attempts first: it is the one closest to that ceiling, so it is
	// the one that can turn terminal, and the early exit below then keeps the
	// other gate's rewind from erasing the escalation.
	focusedAttempts := parseWorkflowInt(nonNilVars(wfExec)[autoFixCounterKey(focusedStep)])
	verifyAttempts := parseWorkflowInt(nonNilVars(wfExec)[autoFixCounterKey(verifyStep)])

	// Both applies may independently rewind to the same "implement" step via
	// rewindRetry, which clears that step's StepRecords as part of arming its
	// rewind. Whichever apply runs first, if it fires, wipes the record the
	// second apply's own precondition depends on (wfExec.CountStep(
	// verifyChecksImplStepID) != 0 — "is there an implementation step to
	// rewind into") — so restore a marker record before the second call if
	// the first one cleared it, otherwise a genuine double-failure would
	// incorrectly look like "nothing to rewind into" to the second gate and
	// escalate straight to human-required instead of joining the merged
	// rewind.
	hadImplementStep := wfExec != nil && wfExec.CountStep(verifyChecksImplStepID) != 0
	restoreImplementMarker := func() {
		if hadImplementStep && wfExec != nil && wfExec.CountStep(verifyChecksImplStepID) == 0 {
			now := e.now()
			wfExec.RecordStep(StepRecord{StepID: verifyChecksImplStepID, Status: "completed", StartedAt: now, EndedAt: now})
		}
	}

	applyFocused := func() (StepOutput, error) {
		return e.applyFocusedChecksVerdict(taskID, focusedStep, wfExec, t, focusedVerdict)
	}
	applyVerify := func() (StepOutput, error) {
		return e.applyVerifyChecksVerdict(taskID, verifyStep, wfExec, t, verifyVerdict)
	}

	first, second := applyFocused, applyVerify
	if verifyAttempts > focusedAttempts {
		first, second = applyVerify, applyFocused
	}

	firstOut, firstErr := first()
	if firstErr != nil && !errors.Is(firstErr, errStepParked) {
		return StepOutput{}, firstErr
	}
	if out, terminal := coordinatorTerminalOutput(step, firstOut); terminal {
		// This gate escalated instead of rewinding (auto-fix ceiling reached,
		// or nothing to rewind into). Stop here: the other gate's rewind would
		// overwrite the terminal status with the stale pre-gate one.
		return out, nil
	}

	priorRetryAfter := ""
	if errors.Is(firstErr, errStepParked) {
		priorRetryAfter = nonNilVars(wfExec)[workflowRetryAfterVar]
	}
	restoreImplementMarker()
	secondOut, secondErr := second()
	if secondErr != nil && !errors.Is(secondErr, errStepParked) {
		return StepOutput{}, secondErr
	}
	keepLaterRetryAfter(wfExec, priorRetryAfter)

	if out, terminal := coordinatorTerminalOutput(step, secondOut); terminal {
		return out, nil
	}
	if errors.Is(firstErr, errStepParked) || errors.Is(secondErr, errStepParked) {
		return StepOutput{}, errStepParked
	}

	return StepOutput{StepID: step.ID, Status: "completed", Output: "clean"}, nil
}

// applyTerminalGates applies only the gates whose verdict is terminal, in the
// order that lands the documented precedence on the task's final status:
// focused_checks' human-required first, then verify_checks, so a verify
// "blocked" write wins. A non-terminal gate's verdict is deliberately never
// applied here — its rewind would overwrite the terminal status with the stale
// pre-gate one, and the serial chain likewise never ran a later gate once an
// earlier one escalated.
func (e *Engine) applyTerminalGates(
	taskID string, step *Step, wfExec *Execution, t TaskInfo,
	focusedStep *Step, focusedVerdict focusedChecksVerdict, focusedTerminal bool,
	verifyStep *Step, verifyVerdict verifyChecksVerdict, verifyTerminal bool,
) (StepOutput, error) {
	out := StepOutput{StepID: step.ID, Status: "completed", Output: "human-required: gate flagged"}
	if focusedTerminal {
		if _, err := e.applyFocusedChecksVerdict(taskID, focusedStep, wfExec, t, focusedVerdict); err != nil {
			return StepOutput{}, err
		}
	}
	if verifyTerminal {
		verifyOut, err := e.applyVerifyChecksVerdict(taskID, verifyStep, wfExec, t, verifyVerdict)
		if err != nil {
			return StepOutput{}, err
		}
		if terminalOut, terminal := coordinatorTerminalOutput(step, verifyOut); terminal {
			out = terminalOut
		}
	}
	return out, nil
}

// focusedVerdictTerminal reports whether applying v is guaranteed to write a
// terminal status rather than possibly arming a rewind. It mirrors
// applyFocusedChecksVerdict's branches: only the run-error paths (exceeded the
// time budget, or the checks could not run at all) flag unconditionally. A
// failed command goes through the rewind path, which turns terminal only when
// the rewind itself is refused — handled by the apply ordering, not here.
func (e *Engine) focusedVerdictTerminal(v focusedChecksVerdict) bool {
	if v.err != nil || v.skip != "" || v.runErr == nil {
		return false
	}
	// Engine-shutdown cancellation fails open to a skip, same as the apply.
	return !e.isShutdownCancel(v.runErr)
}

// isShutdownCancel reports whether err is the engine tearing itself down, the
// one run error both gates' applies fail OPEN on (a skip, not a flag).
func (e *Engine) isShutdownCancel(err error) bool {
	return errors.Is(err, context.Canceled) && e.ctx.Err() != nil
}

// verifyVerdictTerminal is focusedVerdictTerminal's counterpart for
// verify_checks, mirroring applyVerifyChecksVerdict: a run error flags (except
// engine-shutdown cancellation) and a classified non-auto-fixable failure
// blocks. Only the auto-fixable failure path can rewind.
func (e *Engine) verifyVerdictTerminal(v verifyChecksVerdict) bool {
	if v.runErr != nil {
		return !e.isShutdownCancel(v.runErr)
	}
	return v.report.FailedCmd != "" && v.classification != nil && !v.classification.AutoFixable
}

// coordinatorTerminalOutput maps a gate's own StepOutput onto the
// coordinator's output whenever that gate wrote a terminal task status.
func coordinatorTerminalOutput(step *Step, gateOut StepOutput) (StepOutput, bool) {
	switch gateOut.Output {
	case "blocked":
		return StepOutput{StepID: step.ID, Status: "completed", Output: "blocked: verify_checks"}, true
	case "flagged":
		return StepOutput{StepID: step.ID, Status: "completed", Output: "human-required: gate flagged"}, true
	}
	return StepOutput{}, false
}

// keepLaterRetryAfter restores prior when the second gate's rewind replaced it
// with an earlier time. Both rewinds write the same shared retry-after var,
// and the gate with more prior auto-fix attempts — which applies first — is
// the one whose (larger) backoff must survive the merged rewind.
func keepLaterRetryAfter(wfExec *Execution, prior string) {
	if wfExec == nil || prior == "" {
		return
	}
	priorAt, err := time.Parse(time.RFC3339, prior)
	if err != nil {
		return
	}
	current, ok := workflowRetryAfter(wfExec)
	if !ok || priorAt.After(current) {
		wfExec.SetVar(workflowRetryAfterVar, prior)
	}
}

func autoFixCounterKey(step *Step) string { return "step." + step.ID + ".auto_fix" }

func nonNilVars(wfExec *Execution) map[string]string {
	if wfExec == nil {
		return nil
	}
	return wfExec.Variables
}
