package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// codexHookEvents are the low-frequency lifecycle events for which Sybra
// registers observe-only hooks on codex invocations. Per-tool-call events
// (PreToolUse, PostToolUse) are explicitly excluded — only session and
// subagent boundaries are instrumented to keep hooks off the critical path.
var codexHookEvents = []string{
	"SessionStart",
	"SubagentStart",
	"SubagentStop",
	"Stop",
}

// resolveCodexHookBin returns the sybra-cli binary name or path to use in a
// codex hook command string. A bare "sybra-cli" on PATH is preferred — no path
// interpolation occurs. If not found on PATH, the directory of the running
// executable is checked. Returns ("", false) if no shell-safe path is
// resolvable; the caller must omit hooks in that case (fail-open).
func resolveCodexHookBin() (string, bool) {
	if _, err := exec.LookPath("sybra-cli"); err == nil {
		return "sybra-cli", true
	}
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	adjacent := filepath.Join(filepath.Dir(self), "sybra-cli")
	if _, err := os.Stat(adjacent); err != nil {
		return "", false
	}
	// Reject paths with characters that require shell quoting — the hook
	// command string is passed verbatim to codex which runs it via a shell.
	if !safeArgRe.MatchString(adjacent) {
		return "", false
	}
	return adjacent, true
}

// buildCodexHookArgs returns the -c override pairs and
// --dangerously-bypass-hook-trust flag to append to a codex exec invocation.
// If sybra-cli cannot be resolved to a shell-safe path, or taskID fails the
// allowlist check, an empty slice is returned (fail-open — the run proceeds
// without hooks rather than erroring).
func buildCodexHookArgs(taskID string) []string {
	if !safeArgRe.MatchString(taskID) {
		return nil
	}
	bin, ok := resolveCodexHookBin()
	if !ok {
		return nil
	}

	args := make([]string, 0, len(codexHookEvents)*2+1)
	for _, event := range codexHookEvents {
		cmd := fmt.Sprintf("%s hook %s --task %s", bin, event, taskID)
		// TOML inline-table value for the hooks.<Event> config key.
		// Outer array: hook-filter entry. Inner hooks: the action list.
		// run_mode=background keeps hooks off the critical path.
		val := fmt.Sprintf(
			`hooks.%s=[{hooks=[{type="command",command=%q,run_mode="background",timeout_seconds=5}]}]`,
			event, cmd,
		)
		args = append(args, "-c", val)
	}
	args = append(args, "--dangerously-bypass-hook-trust")
	return args
}
