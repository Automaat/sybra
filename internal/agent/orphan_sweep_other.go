//go:build !linux

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

func listProviderProcessesUnderRoots(roots []string) []providerProcess {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	procs := make([]providerProcess, 0)
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		cmd := fields[1]
		if !isProviderProcessName(cmd) {
			continue
		}
		cwdOut, err := exec.CommandContext(context.Background(), "lsof", "-a", "-d", "cwd", "-Fn", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			continue
		}
		cwd := parseLsofCWD(string(cwdOut))
		if !pathWithinRoots(cwd, roots) {
			continue
		}
		procs = append(procs, providerProcess{PID: pid, Command: cmd, CWD: cwd})
	}
	return procs
}
