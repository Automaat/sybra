// Package sysopen opens a local directory in the OS file manager.
package sysopen

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Dir opens path in the platform file manager.
// Supported platforms: linux (xdg-open), darwin (open), windows (explorer).
// Returns a wrapped error if the command exits non-zero or is not found.
func Dir(ctx context.Context, path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", path)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", path)
	case "windows":
		cmd = exec.CommandContext(ctx, "explorer", path)
	default:
		return fmt.Errorf("sysopen: unsupported OS %q", runtime.GOOS)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sysopen: %w", err)
	}
	return nil
}
