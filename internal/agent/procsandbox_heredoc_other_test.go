//go:build !darwin

package agent

import "testing"

// narrowSandboxTempRoot moves the process temp root off /tmp for one test.
//
// CI leaves TMPDIR unset, so the granted temp root would be /tmp, which
// already contains zsh's compiled default prefix, and every assertion here
// would hold with the fix reverted.
func narrowSandboxTempRoot(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}
