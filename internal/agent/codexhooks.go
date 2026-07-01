package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// codexHookEvents are the low-frequency lifecycle events for which Sybra
// registers observe-only hooks on codex invocations. Command validation is
// handled separately by a foreground PreToolUse klaudiush hook.
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
	return resolveHookBin("sybra-cli")
}

func resolveKlaudiushHookBin() (string, bool) {
	return resolveHookBin("klaudiush")
}

func resolveHookBin(name string) (string, bool) {
	if _, err := exec.LookPath(name); err == nil {
		return name, true
	}
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	adjacent := filepath.Join(filepath.Dir(self), name)
	if _, err := exec.LookPath(adjacent); err != nil {
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
// If hook binaries cannot be resolved to shell-safe paths, or taskID fails the
// allowlist check, those hooks are omitted (fail-open — the run proceeds
// without hooks rather than erroring).
func buildCodexHookArgs(taskID string) []string {
	if !safeArgRe.MatchString(taskID) {
		return nil
	}

	args := make([]string, 0, (len(codexHookEvents)+1)*2+1)

	if bin, ok := resolveKlaudiushHookBin(); ok {
		args = append(args, "-c", codexCommandHookValue(
			"PreToolUse",
			bin+" --provider codex --event PreToolUse",
			"",
			30,
		))
	}

	if bin, ok := resolveCodexHookBin(); ok {
		for _, event := range codexHookEvents {
			cmd := fmt.Sprintf("%s hook %s --task %s", bin, event, taskID)
			args = append(args, "-c", codexCommandHookValue(event, cmd, "background", 5))
		}
	}
	if len(args) == 0 {
		return nil
	}
	args = append(args, "--dangerously-bypass-hook-trust")
	return args
}

func codexCommandHookValue(event, command, runMode string, timeoutSeconds int) string {
	// TOML inline-table value for the hooks.<Event> config key.
	// Outer array: hook-filter entry. Inner hooks: the action list.
	if runMode == "" {
		return fmt.Sprintf(
			`hooks.%s=[{hooks=[{type="command",command=%q,timeout_seconds=%d}]}]`,
			event, command, timeoutSeconds,
		)
	}
	return fmt.Sprintf(
		`hooks.%s=[{hooks=[{type="command",command=%q,run_mode=%q,timeout_seconds=%d}]}]`,
		event, command, runMode, timeoutSeconds,
	)
}
