//go:build linux

package agent

import (
	"os"
	"strconv"
)

// processGroupActive reports whether a process group still has a member that
// can execute. Zombies keep a PGID visible to kill(2) but cannot mutate a
// worktree; treating a zombie-only group as live would permanently fence the
// attempt on hosts whose PID 1 does not promptly reap orphans (notably test
// containers).
func processGroupActive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // observation failed: keep ownership fenced
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		fields := procStatFields(data)
		if len(fields) < 3 || fields[0] == "Z" {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err == nil && group == pgid {
			return true
		}
	}
	return false
}
