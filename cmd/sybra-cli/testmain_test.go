package main

import (
	"os"
	"testing"
)

// TestMain force-clears SYBRA_CONTROL_HOME before any test in this package
// runs. cmd/sybra-cli's run() gives SYBRA_CONTROL_HOME precedence over
// SYBRA_HOME by design, so bare sybra-cli calls from inside a task-scoped
// agent reach the real operator board. An ambient SYBRA_CONTROL_HOME is
// exactly the environment any Sybra-dispatched agent runs in — including one
// iterating on this package's own tests — so a test that isolates itself
// with only t.Setenv("SYBRA_HOME", ...) still leaks onto the real board
// (#2136). Clearing it once here, rather than patching each test, means a
// future test can't reintroduce the same leak by simply forgetting to do so.
// Tests that specifically need SYBRA_CONTROL_HOME set (e.g.
// TestControlHomeEnv_WinsOverSybraHome) opt back in with their own
// t.Setenv, which layers on top of this cleanly.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("SYBRA_CONTROL_HOME")
	os.Exit(m.Run())
}
