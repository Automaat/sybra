package agent

import (
	"math"
	"runtime"
	"sync"
	"time"
)

// scaledDeadline extends base to absorb background load on the host running
// the test - e.g. a pre-push `go test -race ./...` competing with Sybra's own
// fleet of agent processes for CPU. Unlike the e2e suite's timeout scaling
// (internal/sybra/e2e_workflow_test.go), this is unconditional: it is not
// gated behind CI/GITHUB_ACTIONS, since local dev machines (darwin in
// particular) see the same oversubscription CI does.
func scaledDeadline(base time.Duration) time.Duration {
	return time.Duration(int64(base) * hostOversubscriptionFactor())
}

// hostOversubscriptionCeiling caps scaledDeadline's multiplier so a runaway
// or misread load figure can't turn a genuine deadlock into a multi-minute
// hang before the test fails.
const hostOversubscriptionCeiling = 8

// hostOversubscriptionFactorCached memoizes hostOversubscriptionFactor so the
// many scaledDeadline call sites sprinkled through tight polling loops don't
// each shell out to read the load average - that per-call subprocess spawn
// (darwin's sysctl) perturbs goroutine scheduling enough to change what a
// test actually exercises.
var hostOversubscriptionFactorCached struct {
	once  sync.Once
	value int64
}

func hostOversubscriptionFactor() int64 {
	hostOversubscriptionFactorCached.once.Do(func() {
		hostOversubscriptionFactorCached.value = hostOversubscriptionFactorResolve()
	})
	return hostOversubscriptionFactorCached.value
}

func hostOversubscriptionFactorResolve() int64 {
	load, ok := hostLoadPerCPU()
	if !ok || load <= 1 {
		return 1
	}
	factor := int64(math.Ceil(load))
	if factor > hostOversubscriptionCeiling {
		return hostOversubscriptionCeiling
	}
	return factor
}

// loadPerCPU divides a 1-minute load average by CPU count. Shared by the
// per-OS hostLoadPerCPU implementations (loadscale_linux_test.go,
// loadscale_darwin_test.go).
func loadPerCPU(load1 float64) (float64, bool) {
	cpus := runtime.NumCPU()
	if cpus <= 0 {
		return 0, false
	}
	return load1 / float64(cpus), true
}
