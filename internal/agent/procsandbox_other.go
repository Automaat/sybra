//go:build !darwin

package agent

import "fmt"

// sandboxExecAvailable always reports false on non-darwin: sandbox-exec is a
// macOS-only mechanism. Server/Linux OS-level enforcement (e.g. bwrap or a
// per-agent container) is out of scope here and tracked as a follow-up
// (#1595).
func sandboxExecAvailable() bool { return false }

// materializeSandboxProfile has nothing to materialize on non-darwin.
func materializeSandboxProfile() (string, error) {
	return "", fmt.Errorf("sandbox: sandbox-exec is darwin-only")
}

// canonicalizeRoot is unused on non-darwin (injectProcessSandbox never
// reaches canonicalization here: sandboxExecAvailable is always false, so
// enforce mode fails closed before any root is resolved).
func canonicalizeRoot(root string) (string, error) {
	return "", fmt.Errorf("sandbox: sandbox-exec is darwin-only")
}

// wrapInvocation is a no-op passthrough on non-darwin.
func wrapInvocation(name string, args []string, _ *RunConfig) (wrappedName string, wrappedArgs []string) {
	return name, args
}
