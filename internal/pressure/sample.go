// Package pressure gates new agent dispatch on local resource pressure
// (disk, memory, CPU load) so a saturated host defers expensive work instead
// of piling more agents onto a machine that is already struggling.
package pressure

import "math"

// Sample captures a point-in-time read of local resource pressure signals.
// NaN in any field means that signal could not be read (unsupported OS,
// missing /proc, a failed syscall) — an unreadable signal is never itself
// treated as pressure; see thresholdTripped.
type Sample struct {
	DiskFreePct     float64
	MemAvailablePct float64
	LoadPerCPU      float64
}

// thresholdTripped reports whether value has crossed threshold in the
// configured "bad" direction. below=true means lower-is-worse (a percent-free
// signal like disk/memory); below=false means higher-is-worse (a load
// signal). threshold<=0 disables the check ("skip" — the operator left this
// dimension unconfigured). A NaN value — the signal could not be read — also
// always skips: an absent signal must never itself be treated as pressure.
func thresholdTripped(value, threshold float64, below bool) bool {
	if threshold <= 0 || math.IsNaN(value) {
		return false
	}
	if below {
		return value < threshold
	}
	return value > threshold
}
