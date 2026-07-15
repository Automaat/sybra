//go:build !darwin && !linux

package agent

import "fmt"

// sandboxExecAvailable always reports false on non-darwin/non-linux hosts:
// Sybra only supports OS-level sandbox enforcement via sandbox-exec on macOS
// and bwrap on Linux.
func sandboxExecAvailable() bool { return false }

func sandboxWrapperName() string { return "host sandbox wrapper" }

// materializeSandboxProfile has nothing to materialize on non-darwin.
func materializeSandboxProfile() (string, error) {
	return "", fmt.Errorf("sandbox: OS-level sandbox unsupported on this host")
}

// canonicalizeRoot is unused on non-darwin (injectProcessSandbox never
// reaches canonicalization here: sandboxExecAvailable is always false, so
// enforce mode fails closed before any root is resolved).
func canonicalizeRoot(root string) (string, error) {
	return "", fmt.Errorf("sandbox: OS-level sandbox unsupported on this host")
}

// wrapInvocation is a no-op passthrough on non-darwin.
func wrapInvocation(name string, args []string, _ *RunConfig) (wrappedName string, wrappedArgs []string) {
	return name, args
}
