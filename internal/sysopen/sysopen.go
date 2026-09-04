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
	return run(ctx, path)
}

// URL opens rawURL in the platform's default browser.
//
// The webview the desktop UI runs in has no window-opening delegate, so
// window.open there is a silent no-op — an external link has to leave through
// the host instead.
func URL(ctx context.Context, rawURL string) error {
	return run(ctx, rawURL)
}

func run(ctx context.Context, target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", target)
	case "windows":
		cmd = exec.CommandContext(ctx, "explorer", target)
	default:
		return fmt.Errorf("sysopen: unsupported OS %q", runtime.GOOS)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sysopen: %w", err)
	}
	return nil
}
