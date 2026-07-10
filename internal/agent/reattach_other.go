//go:build !linux

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

func processStartString(ctx context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
