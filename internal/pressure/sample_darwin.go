//go:build darwin

package pressure

import (
	"encoding/binary"
	"math"
	"runtime"

	"golang.org/x/sys/unix"
)

// readSample takes a best-effort point-in-time reading of disk, memory, and
// load pressure. Every signal is independent: a failure reading one (e.g. a
// missing sysctl on a locked-down CI image) never prevents the others from
// reporting, and never itself counts as pressure — it surfaces as NaN.
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

// memAvailablePct approximates macOS's notion of "available" memory as
// free+inactive pages over total physical memory. Darwin aggressively uses
// free RAM for filesystem cache, so a raw free-page count alone would
// false-trigger pressure under completely normal operation — inactive pages
// are reclaimable the same way Linux's MemAvailable counts reclaimable cache.
func memAvailablePct() float64 {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return math.NaN()
	}
	pageSize, err := unix.SysctlUint32("hw.pagesize")
	if err != nil || pageSize == 0 {
		return math.NaN()
	}
	free, ferr := unix.SysctlUint32("vm.page_free_count")
	inactive, ierr := unix.SysctlUint32("vm.page_inactive_count")
	if ferr != nil || ierr != nil {
		return math.NaN()
	}
	available := uint64(free+inactive) * uint64(pageSize)
	return float64(available) / float64(total) * 100
}

// loadPerCPU reads the BSD `struct loadavg` (vm.loadavg) — three fixed-point
// load averages scaled by fscale, of which only the 1-minute figure is used
// — and normalizes it by CPU count so the configured threshold is portable
// across differently-sized hosts.
//
//	struct loadavg {
//	    fixpt_t ldavg[3]; // 3x uint32, offset 0
//	    long    fscale;   // 8-byte aligned, offset 16 on LP64
//	};
func loadPerCPU() float64 {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return math.NaN()
	}
	ld0 := binary.LittleEndian.Uint32(raw[0:4])
	fscale := binary.LittleEndian.Uint64(raw[16:24])
	if fscale == 0 {
		return math.NaN()
	}
	cpus := runtime.NumCPU()
	if cpus <= 0 {
		return math.NaN()
	}
	load1 := float64(ld0) / float64(fscale)
	return load1 / float64(cpus)
}
