// Package sysopen opens a local directory in the OS file manager.
package sysopen

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Dir opens path in the platform file manager.
// Supported platforms: linux (xdg-open), darwin (open), windows (explorer).
// Returns a wrapped error if the command exits non-zero or is not found.
func Dir(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", path)
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		return fmt.Errorf("sysopen: unsupported OS %q", runtime.GOOS)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sysopen: %w", err)
	}
	return nil
}
