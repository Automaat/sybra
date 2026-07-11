//go:build darwin

package agent

import (
	"os/exec"
	"strconv"
	"strings"
)

// hostLoadPerCPU reads the 1-minute load average via `sysctl -n vm.loadavg`
// (darwin has no /proc/loadavg). Output looks like "{ 1.23 1.09 1.15 }".
func hostLoadPerCPU() (float64, bool) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(fields) == 0 {
		return 0, false
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return loadPerCPU(load1)
}
