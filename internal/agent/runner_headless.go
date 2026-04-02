package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

func (m *Manager) runHeadless(ctx context.Context, a *Agent, prompt string, allowedTools []string) {
	args := []string{"-p", prompt, "--output-format", "stream-json"}

	if a.SessionID != "" {
		args = append(args, "--resume", a.SessionID)
	}

	if len(allowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowedTools, ","))
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}

	cmd := exec.CommandContext(ctx, "claude", args...)
	a.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.handleError(a, fmt.Errorf("stdout pipe: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		m.handleError(a, fmt.Errorf("start claude: %w", err))
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var event StreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}

		a.outputBuffer = append(a.outputBuffer, event)
		m.emit("agent:output:"+a.ID, event)

		if event.Type == "result" {
			a.SessionID = event.SessionID
			a.CostUSD += event.CostUSD
		}
	}

	_ = cmd.Wait()

	a.State = StateStopped
	m.emit("agent:state:"+a.ID, a)
}

func (m *Manager) handleError(a *Agent, err error) {
	a.State = StateStopped
	m.emit("agent:error:"+a.ID, err.Error())
}
