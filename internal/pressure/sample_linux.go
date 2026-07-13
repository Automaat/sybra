//go:build linux

package pressure

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// readSample takes a best-effort point-in-time reading of disk, memory, and
// load pressure. Every signal is independent: a failure reading one (e.g. an
// unreadable /proc/meminfo) never prevents the others from reporting, and
// never itself counts as pressure — it surfaces as NaN.
func readSample(probeDir string) Sample {
	return Sample{
		DiskFreePct:     diskFreePct(probeDir),
		MemAvailablePct: memAvailablePct(),
		LoadPerCPU:      loadPerCPU(),
	}
}

func diskFreePct(dir string) float64 {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil || st.Blocks == 0 {
		return math.NaN()
	}
	return float64(st.Bavail) / float64(st.Blocks) * 100
}

// memAvailablePct reads /proc/meminfo's MemAvailable (the kernel's own
// estimate of memory available for new allocations without swapping,
// accounting for reclaimable caches) against MemTotal.
func memAvailablePct() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return math.NaN()
	}
	var total, avail float64
	haveTotal, haveAvail := false, false
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if v, pErr := strconv.ParseFloat(fields[1], 64); pErr == nil {
				total = v
				haveTotal = true
			}
		case "MemAvailable:":
			if v, pErr := strconv.ParseFloat(fields[1], 64); pErr == nil {
				avail = v
				haveAvail = true
			}
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveTotal || !haveAvail || total == 0 {
		return math.NaN()
	}
	return avail / total * 100
}

// loadPerCPU reads /proc/loadavg's 1-minute load average, normalized by CPU
// count so the configured threshold is portable across differently-sized
// hosts.
func loadPerCPU() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return math.NaN()
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return math.NaN()
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return math.NaN()
	}
	cpus := runtime.NumCPU()
	if cpus <= 0 {
		return math.NaN()
	}
	return load1 / float64(cpus)
}
