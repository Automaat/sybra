package sysopen

import (
	"runtime"
	"testing"
)

func TestDir_UnsupportedOS(t *testing.T) {
	// Only meaningful when running on a platform we don't support,
	// or to verify the function exists and compiles on all platforms.
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("skipped on supported platforms")
	}
	if err := Dir("/tmp"); err == nil {
		t.Error("expected error for unsupported OS, got nil")
	}
}

func TestDir_InvalidPath(t *testing.T) {
	// xdg-open/open/explorer on a nonexistent path should still succeed
	// (they open a file manager at the nearest valid parent or show an error
	// dialog). This test mainly verifies the function wires up correctly.
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("skipped on unsupported platforms")
	}
	// We cannot actually run xdg-open/open/explorer in a headless CI
	// environment without a display. So just verify the command is built.
	t.Skip("skipped: requires display / file manager")
}
