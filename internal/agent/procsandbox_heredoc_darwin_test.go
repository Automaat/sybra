//go:build darwin

package agent

import "testing"

// narrowSandboxTempRoot moves the process temp root off /tmp for one test.
//
// A host that leaves TMPDIR unset grants /tmp as the temp root, which already
// contains zsh's compiled default prefix, and every assertion here would hold
// with the fix reverted.
func narrowSandboxTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}
