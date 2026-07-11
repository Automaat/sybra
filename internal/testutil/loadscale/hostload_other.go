//go:build !linux && !darwin

package loadscale

// HostLoadPerCPU has no known load-average source on this OS.
func HostLoadPerCPU() (float64, bool) {
	return 0, false
}
