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

var workflowCheckLoadPerCPU = pressure.CurrentLoadPerCPU

// resolveWorkflowCheckTimeout derives the effective timeout budget for
// deterministic local workflow checks. On an oversubscribed host, a fixed
// wall-clock budget misclassifies scheduler starvation as a product failure,
// so the budget scales with live load per CPU instead.
func resolveWorkflowCheckTimeout(base time.Duration) time.Duration {
	if base <= 0 {
		base = verifyChecksDefaultTimeout
	}
	load, ok := workflowCheckLoadPerCPU()
	if !ok || load <= 1 {
		return base
	}
	factor := min(int64(math.Ceil(load)), verifyTimeoutScaleCeiling)
	return time.Duration(int64(base) * factor)
}
