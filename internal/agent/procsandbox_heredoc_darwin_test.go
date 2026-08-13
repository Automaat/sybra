//go:build darwin

package agent

import "testing"

// narrowSandboxTempRoot moves the process temp root off /tmp for one test.
//
// A host that leaves TMPDIR unset grants /tmp as the temp root, which already
// contains zsh's compiled default prefix, and every assertion here would hold
// with the fix reverted. The shared profile is materialized before the move,
// because darwin caches its path for the process lifetime and a later test
// would otherwise reach for it inside a directory this one deleted.
func narrowSandboxTempRoot(t *testing.T) {
	t.Helper()
	if _, err := materializeSandboxProfile(); err != nil {
		t.Skipf("sandbox profile unavailable on this host: %v", err)
	}
	t.Setenv("TMPDIR", t.TempDir())
}
