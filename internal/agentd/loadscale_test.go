package agentd

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/testutil/loadscale"
)

// scaledDeadline extends base to absorb background load on the host running
// the test. These deadlines cover a poll interval, a real git clone into the
// run's workspace, a provider spawn, and the spool flush — a chain that takes
// milliseconds on an idle machine and seconds under a `go test -race ./...`
// that has every core busy. The unscaled 5s budget failed there with an empty
// event list, i.e. the run had not started yet rather than misbehaved.
//
// Set SYBRA_AGENTD_TIMEOUT_SCALE=1 to force unscaled deadlines while debugging
// timing locally.
func scaledDeadline(base time.Duration) time.Duration {
	return loadscale.ScaleDuration(base, agentdOversubscriptionFactor())
}

// agentdOversubscriptionCeiling caps the multiplier so a runaway or misread
// load figure cannot turn a genuine deadlock into a multi-minute hang before
// the test fails.
const agentdOversubscriptionCeiling = 8

// agentdOversubscriptionFactorCached memoizes the factor: the deadline helpers
// sit in tight polling loops, and reading the load average per call shells out
// on darwin, which perturbs the very scheduling these tests measure.
var agentdOversubscriptionFactorCached struct {
	once  sync.Once
	value int64
}

func agentdOversubscriptionFactor() int64 {
	agentdOversubscriptionFactorCached.once.Do(func() {
		agentdOversubscriptionFactorCached.value = agentdOversubscriptionFactorResolve()
	})
	return agentdOversubscriptionFactorCached.value
}

func agentdOversubscriptionFactorResolve() int64 {
	return agentdOversubscriptionFactorResolveWith(
		os.Getenv("SYBRA_AGENTD_TIMEOUT_SCALE"),
		func() int64 { return loadscale.HostOversubscriptionFactor(agentdOversubscriptionCeiling) },
	)
}

func agentdOversubscriptionFactorResolveWith(envValue string, hostFactor func() int64) int64 {
	if v := strings.TrimSpace(envValue); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return hostFactor()
}
