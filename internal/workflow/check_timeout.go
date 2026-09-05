package workflow

import (
	"math"
	"time"

	"github.com/Automaat/sybra/internal/pressure"
)

// verifyTimeoutScaleCeiling caps how far host-load scaling can stretch a
// deterministic workflow check budget. A runaway load reading must not turn a
// genuine hang into an effectively unbounded wait.
const verifyTimeoutScaleCeiling int64 = 8

// verifyTimeoutScaleCeilingLoad is the load-per-CPU at which scaling saturates.
// Tests that must exercise the widest budget stub this rather than an arbitrary
// value, so a change to the ceiling keeps them in step.
const verifyTimeoutScaleCeilingLoad = float64(verifyTimeoutScaleCeiling)

var workflowCheckLoadPerCPU = pressure.CurrentLoadPerCPU

// resolveWorkflowCheckTimeout derives the effective timeout budget for
// deterministic local workflow checks. On an oversubscribed host, a fixed
// wall-clock budget misclassifies scheduler starvation as a product failure,
// so the budget scales with live load per CPU instead.
func resolveWorkflowCheckTimeout(base time.Duration) time.Duration {
	return scaleWorkflowCheckTimeout(baseWorkflowCheckTimeout(base))
}

func baseWorkflowCheckTimeout(base time.Duration) time.Duration {
	if base <= 0 {
		return verifyChecksDefaultTimeout
	}
	return base
}

func scaleWorkflowCheckTimeout(base time.Duration) time.Duration {
	load, ok := workflowCheckLoadPerCPU()
	if !ok || load <= 1 {
		return base
	}
	factor := min(int64(math.Ceil(load)), verifyTimeoutScaleCeiling)
	return time.Duration(int64(base) * factor)
}

// deadlineBeatLoad reports whether a run that exhausted budget would have been
// granted more had the host's load been sampled when it finished rather than
// when it started.
//
// The budget is fixed once, before the work begins, but the oversubscription
// it compensates for develops during it: a verify run that starts on an idle
// machine and ends on one carrying several agents keeps the idle budget and
// reports a timeout the scaling existed to prevent. Re-reading the load at the
// deadline costs one sysctl and distinguishes "this suite hangs" from "this
// host filled up while the suite ran".
func deadlineBeatLoad(base, spent time.Duration) (scaled time.Duration, starved bool) {
	scaled = scaleWorkflowCheckTimeout(baseWorkflowCheckTimeout(base))
	return scaled, spent < scaled
}
