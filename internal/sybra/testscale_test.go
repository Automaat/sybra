package sybra

import (
	"os"
	"strconv"
	"strings"

	"github.com/Automaat/sybra/internal/testutil/loadscale"
)

// Deadline scaling for every test in this package, tagged or not.
//
// It lives in an untagged file because the unscaled waits that motivated
// #2811 were in untagged tests, which cannot see anything defined behind
// //go:build e2e. Keeping one definition here is what stops a helper from
// quietly growing its own hardcoded budget again.

func e2eTimeoutScale() int64 {
	return e2eTimeoutScaleResolve()
}

const (
	e2eTimeoutScaleCeiling = 20
	e2eCITimeoutScaleFloor = 12
)

func e2eTimeoutScaleResolve() int64 {
	if v := strings.TrimSpace(os.Getenv("SYBRA_E2E_TIMEOUT_SCALE")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	factor := loadscale.HostOversubscriptionFactor(e2eTimeoutScaleCeiling)
	// CI runners carry a known-bad baseline (slow fork/exec, container I/O
	// variance) even when the load average looks idle, so they get a fixed
	// floor on top of the measured factor. Local/dev runs (including a fleet
	// of concurrent agents on darwin/linux) have no such baseline — they
	// scale purely off measured host load, same as CI does above the floor.
	if os.Getenv("CI") == "" && os.Getenv("GITHUB_ACTIONS") == "" {
		return factor
	}
	scaled := e2eCITimeoutScaleFloor * factor
	if scaled < e2eCITimeoutScaleFloor {
		return e2eCITimeoutScaleFloor
	}
	if scaled > e2eTimeoutScaleCeiling {
		return e2eTimeoutScaleCeiling
	}
	return scaled
}
