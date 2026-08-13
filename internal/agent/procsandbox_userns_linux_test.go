//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBwrap writes a bwrap stand-in that reproduces the failure an Ubuntu
// 24.04 host produces when it denies the user namespace.
func fakeBwrap(t *testing.T, script string) string {
	t.Helper()
	return writeFakeBwrap(t, t.TempDir(), script)
}

func writeFakeBwrap(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	return path
}

func writeSysctl(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apparmor_restrict_unprivileged_userns")
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatalf("write sysctl: %v", err)
	}
	return path
}

func useSysctl(t *testing.T, value string) {
	t.Helper()
	original := apparmorSysctlPath
	apparmorSysctlPath = writeSysctl(t, value)
	t.Cleanup(func() { apparmorSysctlPath = original })
}

func TestProbeUserNamespaceNamesBlockedNamespace(t *testing.T) {
	tests := []struct {
		name       string
		sysctl     string
		failure    string
		wantSysctl bool
	}{
		{name: "denied namespace with the sysctl on", sysctl: "1\n", failure: "bwrap: setting up uid map: Permission denied", wantSysctl: true},
		{name: "denied namespace with the sysctl off", sysctl: "0\n", failure: "bwrap: setting up uid map: Permission denied", wantSysctl: false},
		// Every bwrap failure exits 1. A failure that is not the namespace
		// must never be answered with "weaken the host-wide restriction".
		{name: "missing target binary", sysctl: "1\n", failure: "bwrap: execvp true: No such file or directory", wantSysctl: false},
		{name: "namespace quota exhausted", sysctl: "1\n", failure: "bwrap: Creating new namespace failed: No space left on device", wantSysctl: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useSysctl(t, tc.sysctl)
			err := probeUserNamespace(fakeBwrap(t, `echo `+shellQuote(tc.failure)+` >&2; exit 1`))
			if err == nil {
				t.Fatal("expected the probe to refuse a wrapper that cannot build a sandbox")
			}
			if !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("probe error %q does not carry the host failure", err)
			}
			if got := strings.Contains(err.Error(), apparmorUserNamespaceSysctl); got != tc.wantSysctl {
				t.Fatalf("naming %s = %v, want %v (error: %v)", apparmorUserNamespaceSysctl, got, tc.wantSysctl, err)
			}
		})
	}
}

// The whole chain: a wrapper that is on PATH but refuses to map uids must not
// certify enforce. This is the Ubuntu 24.04 host shape, where looking for the
// binary alone certified a host that can build no sandbox at all.
func TestSandboxMechanismRefusesWrapperThatCannotMapUIDs(t *testing.T) {
	dir := t.TempDir()
	writeFakeBwrap(t, dir, `echo "bwrap: setting up uid map: Permission denied" >&2; exit 1`)
	t.Setenv("PATH", dir)
	useSysctl(t, "1\n")
	resetSandboxMechanismProbe(t)

	err := sandboxMechanismErr()
	if err == nil {
		t.Fatal("a wrapper that cannot map uids reported the mechanism as available")
	}
	if !strings.Contains(err.Error(), apparmorUserNamespaceSysctl) {
		t.Fatalf("refusal %q does not name the sysctl an operator has to act on", err)
	}
	if sandboxExecAvailable() {
		t.Fatal("sandboxExecAvailable still claims the mechanism")
	}
	observation, err := ProbeSandboxPosture("enforce")
	if err == nil {
		t.Fatalf("enforce certified an uncontainable host: %+v", observation)
	}
	if observation.Available || observation.Contained {
		t.Fatalf("observation claims a mechanism the host denies: %+v", observation)
	}
}

// A transient failure — a namespace quota exhausted by concurrent runs, fork
// pressure, a probe that timed out under load — must not refuse every later
// run for the life of the process. A host that recovers has to certify again
// without an operator restarting the board.
func TestSandboxMechanismReprobesAfterTransientFailure(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "failed-once")
	writeFakeBwrap(t, dir, `if [ ! -f `+shellQuote(marker)+` ]; then : > `+shellQuote(marker)+`; echo "bwrap: Creating new namespace failed: No space left on device" >&2; exit 1; fi; exit 0`)
	t.Setenv("PATH", dir)
	useSysctl(t, "0\n")
	resetSandboxMechanismProbe(t)

	if err := sandboxMechanismErr(); err == nil {
		t.Fatal("expected the first probe to refuse")
	}
	if err := sandboxMechanismErr(); err == nil {
		t.Fatal("a failure must stand until it ages out, so a burst costs one spawn")
	}

	sandboxProbeMu.Lock()
	bwrapProbedAt = bwrapProbedAt.Add(-sandboxProbeRetryAfter - time.Second)
	sandboxProbeMu.Unlock()

	if err := sandboxMechanismErr(); err != nil {
		t.Fatalf("a recovered host still refuses: %v", err)
	}
}

// A missing binary is a static fact, so it is cached outright rather than
// re-execed on every dispatch.
func TestSandboxMechanismCachesMissingWrapper(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resetSandboxMechanismProbe(t)

	err := sandboxMechanismErr()
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("missing wrapper reported as %v", err)
	}
	sandboxProbeMu.Lock()
	aged := bwrapProbedAt.Add(-sandboxProbeRetryAfter - time.Second)
	bwrapProbedAt = aged
	sandboxProbeMu.Unlock()
	if err := sandboxMechanismErr(); err == nil {
		t.Fatal("a missing wrapper must stay missing")
	}
	sandboxProbeMu.Lock()
	reprobed := !bwrapProbedAt.Equal(aged)
	sandboxProbeMu.Unlock()
	if reprobed {
		t.Fatal("a missing wrapper was re-probed instead of cached")
	}
}

// resetSandboxMechanismProbe clears the process-wide cache so a test can
// present a different host. The cache is deliberately kept in production.
func resetSandboxMechanismProbe(t *testing.T) {
	t.Helper()
	reset := func() {
		sandboxProbeMu.Lock()
		defer sandboxProbeMu.Unlock()
		bwrapPath, bwrapErr, bwrapProbed, bwrapMissing = "", nil, false, false
		bwrapProbedAt = time.Time{}
	}
	reset()
	// Leaving the cache cleared makes the next caller re-probe the real host
	// rather than inherit this test's fake wrapper.
	t.Cleanup(reset)
}

func TestProbeUserNamespaceAcceptsWorkingWrapper(t *testing.T) {
	if err := probeUserNamespace(fakeBwrap(t, "exit 0")); err != nil {
		t.Fatalf("probe rejected a working wrapper: %v", err)
	}
}

// A wrapper whose descendant outlives it keeps the inherited output pipe open.
// Waiting on that pipe alone never returns, so the deadline has to close it or
// certification hangs forever and nothing dispatches at all.
func TestProbeUserNamespaceBoundsAWedgedWrapper(t *testing.T) {
	original := sandboxProbeTimeout
	sandboxProbeTimeout = time.Second
	t.Cleanup(func() { sandboxProbeTimeout = original })

	done := make(chan error, 1)
	go func() { done <- probeUserNamespace(fakeBwrap(t, "/bin/sleep 600 &\nwait")) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged wrapper certified the mechanism")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never returned: a grandchild holding the pipe outlives the deadline")
	}
}

func TestProbeUserNamespaceReadsSysctlOnlyWhenPresent(t *testing.T) {
	original := apparmorSysctlPath
	apparmorSysctlPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { apparmorSysctlPath = original })

	if userNamespacesRestricted() {
		t.Fatal("a host without the sysctl must not read as restricted")
	}
	err := probeUserNamespace(fakeBwrap(t, `echo "bwrap: No permissions to creating new namespace" >&2; exit 1`))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), apparmorUserNamespaceSysctl) {
		t.Fatalf("sysctl named on a host that does not carry it: %v", err)
	}
}
