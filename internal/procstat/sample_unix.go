//go:build linux || darwin

package procstat

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const psTimeout = 2 * time.Second

func readProcesses() ([]Process, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()

	out, err := exec.CommandContext(
		ctx,
		"ps",
		"-eo",
		"pid=,pgid=,pcpu=,pmem=,comm=",
	).Output()
	if err != nil {
		return nil, false
	}
	return parseProcessOutput(string(out)), true
}

func parseProcessOutput(out string) []Process {
	processes := make([]Process, 0)
	for line := range strings.SplitSeq(out, "\n") {
		p, ok := parseProcessLine(line)
		if !ok {
			continue
		}
		processes = append(processes, p)
	}
	return processes
}

func parseProcessLine(line string) (Process, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return Process{}, false
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return Process{}, false
	}
	pgid, err := strconv.Atoi(fields[1])
	if err != nil {
		return Process{}, false
	}
	cpu, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return Process{}, false
	}
	mem, err := strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return Process{}, false
	}
	name := strings.Join(fields[4:], " ")
	if name == "" {
		return Process{}, false
	}

	return Process{
		PID:        pid,
		PGID:       pgid,
		Name:       name,
		CPUPercent: cpu,
		MemPercent: mem,
	}, true
}
