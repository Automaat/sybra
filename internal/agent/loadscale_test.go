package agent

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/testutil/loadscale"
)

// scaledDeadline extends base to absorb background load on the host running
// the test - e.g. a pre-push `go test -race ./...` competing with Sybra's own
// fleet of agent processes for CPU. Set SYBRA_AGENT_TIMEOUT_SCALE=1 to force
// unscaled deadlines while debugging timing locally.
func scaledDeadline(base time.Duration) time.Duration {
	return loadscale.ScaleDuration(base, hostOversubscriptionFactor())
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
	return hostOversubscriptionFactorResolveWith(
		os.Getenv("SYBRA_AGENT_TIMEOUT_SCALE"),
		func() int64 { return loadscale.HostOversubscriptionFactor(hostOversubscriptionCeiling) },
	)
}

func hostOversubscriptionFactorResolveWith(envValue string, hostFactor func() int64) int64 {
	if v := strings.TrimSpace(envValue); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return hostFactor()
}
