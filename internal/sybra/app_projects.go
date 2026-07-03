package sybra

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/executil"
)

// openCommandInGhostty opens a Ghostty terminal tab running claudeCmd.
// If dir is non-empty the shell changes to that directory first.
func openCommandInGhostty(dir, claudeCmd string) error {
	shellCmd := claudeCmd
	if dir != "" {
		shellCmd = "cd " + dir + " && " + claudeCmd
	}
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Sybra:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic '%s'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, executil.EscapeAppleScript(shellCmd))
	out, err := exec.CommandContext(context.Background(), "osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}

// copyToClipboard copies text to the macOS system clipboard via pbcopy.
func copyToClipboard(text string) error {
	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func openDirInGhostty(dir string) error {
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Sybra:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'cd %s && exec zsh'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, executil.EscapeAppleScript(dir))
	out, err := exec.CommandContext(context.Background(), "osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}
