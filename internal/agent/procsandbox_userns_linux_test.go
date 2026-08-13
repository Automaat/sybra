//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBwrap writes a bwrap stand-in that reproduces the failure an Ubuntu
// 24.04 host produces when it denies the user namespace.
func fakeBwrap(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bwrap")
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

func TestProbeUserNamespaceNamesBlockedNamespace(t *testing.T) {
	tests := []struct {
		name        string
		sysctl      string
		wantSysctl  bool
		wantMessage string
	}{
		{name: "restricted", sysctl: "1\n", wantSysctl: true, wantMessage: "setting up uid map: Permission denied"},
		{name: "permitted", sysctl: "0\n", wantSysctl: false, wantMessage: "setting up uid map: Permission denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := apparmorSysctlPath
			apparmorSysctlPath = writeSysctl(t, tc.sysctl)
			t.Cleanup(func() { apparmorSysctlPath = original })

			bwrap := fakeBwrap(t, `echo "bwrap: setting up uid map: Permission denied" >&2; exit 1`)
			err := probeUserNamespace(bwrap)
			if err == nil {
				t.Fatal("expected the probe to refuse a host that cannot map uids")
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
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
	if err := os.WriteFile(filepath.Join(dir, "bwrap"),
		[]byte("#!/bin/sh\necho \"bwrap: setting up uid map: Permission denied\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	t.Setenv("PATH", dir)
	original := apparmorSysctlPath
	apparmorSysctlPath = writeSysctl(t, "1\n")
	t.Cleanup(func() { apparmorSysctlPath = original })
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

// resetSandboxMechanismProbe clears the process-wide memo so a test can
// present a different host. The probe is deliberately memoized in production.
func resetSandboxMechanismProbe(t *testing.T) {
	t.Helper()
	reset := func() {
		bwrapProbeOnce = sync.Once{}
		bwrapPath, bwrapErr = "", nil
	}
	reset()
	// Leaving the memo cleared makes the next caller re-probe the real host
	// rather than inherit this test's fake wrapper.
	t.Cleanup(reset)
}

func TestProbeUserNamespaceAcceptsWorkingWrapper(t *testing.T) {
	if err := probeUserNamespace(fakeBwrap(t, "exit 0")); err != nil {
		t.Fatalf("probe rejected a working wrapper: %v", err)
	}
}

// A host that denies the namespace must not certify enforce, and must not be
// reported as contained — a binary on PATH is not evidence of containment.
func TestProbeUserNamespaceReportsMissingSysctlFile(t *testing.T) {
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
