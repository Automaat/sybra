package procstat

import (
	"cmp"
	"slices"
	"time"
)

// Process is a point-in-time snapshot of one process.
type Process struct {
	PID        int     `json:"pid"`
	PGID       int     `json:"pgid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float64 `json:"memPercent"`
	Owned      bool    `json:"owned"`
}

// Summary is the operator-facing process sample attached to health output.
type Summary struct {
	TopCPU    []Process `json:"topCpu"`
	TopMem    []Process `json:"topMem"`
	SampledAt time.Time `json:"sampledAt"`
	Available bool      `json:"available"`
}

var (
	readProcessesFn = readProcesses
	timeNow         = func() time.Time { return time.Now().UTC() }
)

// Sample returns a top-N summary sorted by CPU and memory usage.
func Sample(topN int, owned func(pid, pgid int) bool) Summary {
	processes, available := readProcessesFn()
	summary := Summary{
		SampledAt: timeNow(),
		Available: available,
		TopCPU:    []Process{},
		TopMem:    []Process{},
	}
	if !available || len(processes) == 0 || topN <= 0 {
		return summary
	}
	if owned == nil {
		owned = func(int, int) bool { return false }
	}

	all := append([]Process(nil), processes...)
	for i := range all {
		all[i].Owned = owned(all[i].PID, all[i].PGID)
	}

	summary.TopCPU = append([]Process(nil), all...)
	summary.TopMem = append([]Process(nil), all...)
	slices.SortFunc(summary.TopCPU, compareCPU)
	slices.SortFunc(summary.TopMem, compareMem)
	summary.TopCPU = truncate(summary.TopCPU, topN)
	summary.TopMem = truncate(summary.TopMem, topN)

	return summary
}

func truncate(processes []Process, topN int) []Process {
	if len(processes) <= topN {
		return processes
	}
	return processes[:topN]
}

func compareCPU(a, b Process) int {
	if d := cmp.Compare(b.CPUPercent, a.CPUPercent); d != 0 {
		return d
	}
	if d := cmp.Compare(b.MemPercent, a.MemPercent); d != 0 {
		return d
	}
	if d := cmp.Compare(a.PID, b.PID); d != 0 {
		return d
	}
	return cmp.Compare(a.Name, b.Name)
}

func compareMem(a, b Process) int {
	if d := cmp.Compare(b.MemPercent, a.MemPercent); d != 0 {
		return d
	}
	if d := cmp.Compare(b.CPUPercent, a.CPUPercent); d != 0 {
		return d
	}
	if d := cmp.Compare(a.PID, b.PID); d != 0 {
		return d
	}
	return cmp.Compare(a.Name, b.Name)
}
