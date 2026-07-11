//go:build linux

package loadscale

import (
	"os"
	"strconv"
	"strings"
)

// HostLoadPerCPU reads the 1-minute load average from /proc/loadavg.
func HostLoadPerCPU() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return LoadPerCPU(load1)
}
