//go:build e2e && !linux && !darwin

package sybra

// hostLoadPerCPU has no known load-average source on this OS.
func hostLoadPerCPU() (float64, bool) {
	return 0, false
}
