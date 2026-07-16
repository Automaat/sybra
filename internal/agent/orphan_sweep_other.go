//go:build !linux

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

func listProviderProcessesUnderRoots(ctx context.Context, roots []string) []providerProcess {
	out, err := exec.CommandContext(ctx, "ps", "-eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	type candidate struct {
		pid   int
		cmd   string
		owner processOwner
	}
	candidates := make([]candidate, 0)
	pids := make([]string, 0)
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
		owner := processOwnerFromAnyEnv(fields[2:])
		if owner.AgentID == "" && !isProviderProcessName(cmd) {
			continue
		}
		candidates = append(candidates, candidate{pid: pid, cmd: cmd, owner: owner})
		pids = append(pids, strconv.Itoa(pid))
	}
	if len(candidates) == 0 {
		return nil
	}

	cwds := lsofCWDsByPID(ctx, pids)
	procs := make([]providerProcess, 0, len(candidates))
	for _, cand := range candidates {
		cwd := cwds[cand.pid]
		if !pathWithinRoots(cwd, roots) {
			continue
		}
		procs = append(procs, providerProcess{PID: cand.pid, Command: cand.cmd, CWD: cwd, Owner: cand.owner})
	}
	return procs
}

func lsofCWDsByPID(ctx context.Context, pids []string) map[int]string {
	out := make(map[int]string)
	if len(pids) == 0 {
		return out
	}
	cwdOut, err := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(pids, ",")).Output()
	if err != nil {
		return out
	}
	return parseLsofCWDs(string(cwdOut))
}

func parseLsofCWDs(output string) map[int]string {
	out := make(map[int]string)
	pid := 0
	prev := ""
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(line, "p"); ok {
			if parsed, err := strconv.Atoi(value); err == nil {
				pid = parsed
			} else {
				pid = 0
			}
			prev = line
			continue
		}
		if prev == "fcwd" && strings.HasPrefix(line, "n") && pid > 0 {
			out[pid] = line[1:]
		}
		prev = line
	}
	return out
}
