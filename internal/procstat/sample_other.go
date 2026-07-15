//go:build !linux && !darwin

package procstat

func readProcesses() ([]Process, bool) {
	return nil, false
}
