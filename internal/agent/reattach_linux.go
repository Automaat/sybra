//go:build linux

package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
)

const procStatStarttimeIndexAfterComm = 19

func processStartString(_ context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	fields := procStatFields(data)
	if len(fields) <= procStatStarttimeIndexAfterComm {
		return ""
	}
	return fields[procStatStarttimeIndexAfterComm]
}

func procStatFields(data []byte) []string {
	stat := string(data)
	afterComm := strings.LastIndexByte(stat, ')')
	if afterComm < 0 || afterComm+2 >= len(stat) {
		return nil
	}
	return strings.Fields(stat[afterComm+2:])
}
