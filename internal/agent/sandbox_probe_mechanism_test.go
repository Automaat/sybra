package agent

import (
	"errors"
	"strings"
	"testing"
)

// stubMechanism presents a host whose wrapper cannot build a sandbox on a
// platform where it normally can.
func stubMechanism(t *testing.T, err error) {
	t.Helper()
	original := hostSandboxMechanismErr
	hostSandboxMechanismErr = func() error { return err }
	t.Cleanup(func() { hostSandboxMechanismErr = original })
}

// A host whose wrapper is present but cannot build a sandbox must refuse to
// certify enforce, and the refusal must carry the host reason — an operator
// reading "unavailable" alone cannot tell a missing binary from a kernel that
// denies the namespace.
func TestProbeSandboxPostureCarriesMechanismReason(t *testing.T) {
	blocked := errors.New("bwrap cannot create a namespace on this host: setting up uid map: Permission denied")
	stubMechanism(t, blocked)

	observation, err := ProbeSandboxPosture("enforce")
	if err == nil {
		t.Fatal("enforce certified a host that cannot build the sandbox")
	}
	if !errors.Is(err, blocked) {
		t.Fatalf("enforce refusal drops the host reason: %v", err)
	}
	if observation.Available || observation.Contained {
		t.Fatalf("enforce observation claims a mechanism: %+v", observation)
	}
	if !strings.Contains(observation.Evidence, blocked.Error()) {
		t.Fatalf("evidence %q omits the host reason", observation.Evidence)
	}

	report, err := ProbeSandboxPosture("report")
	if err != nil {
		t.Fatalf("report must never block a run: %v", err)
	}
	if !report.Available || report.Contained {
		t.Fatalf("report must stay available and uncontained: %+v", report)
	}
	if !strings.Contains(report.Evidence, blocked.Error()) {
		t.Fatalf("report evidence %q omits the host reason", report.Evidence)
	}
}

// The dispatch path must agree with certification: enforce fails closed with
// the host reason attached, and report degrades to an unwrapped run rather
// than blocking one.
func TestInjectProcessSandboxHonoursMechanismRefusal(t *testing.T) {
	blocked := errors.New("bwrap cannot build a sandbox on this host: Creating new namespace failed")
	stubMechanism(t, blocked)

	enforce := &RunConfig{Dir: t.TempDir(), SandboxMode: "enforce"}
	err := newPostureManager("enforce").injectProcessSandbox(enforce)
	if err == nil {
		t.Fatal("enforce dispatched into a sandbox the host cannot build")
	}
	if !errors.Is(err, blocked) {
		t.Fatalf("enforce failure drops the host reason: %v", err)
	}

	report := &RunConfig{Dir: t.TempDir(), SandboxMode: "report"}
	if err := newPostureManager("report").injectProcessSandbox(report); err != nil {
		t.Fatalf("report blocked a run: %v", err)
	}
	if report.sandbox.mode != "off" {
		t.Fatalf("report left sandbox.mode = %q, want off: report never wraps the process", report.sandbox.mode)
	}
}

func TestInjectReadOnlyProcessSandboxHonoursMechanismRefusal(t *testing.T) {
	blocked := errors.New("bwrap is not on PATH")
	stubMechanism(t, blocked)

	enforce := &RunConfig{Dir: t.TempDir()}
	err := newPostureManager("report").injectReadOnlyProcessSandbox(enforce, "enforce")
	if err == nil || !errors.Is(err, blocked) {
		t.Fatalf("enforce read-only path returned %v, want the host reason", err)
	}

	report := &RunConfig{Dir: t.TempDir()}
	if err := newPostureManager("report").injectReadOnlyProcessSandbox(report, "report"); err != nil {
		t.Fatalf("report blocked a verification command: %v", err)
	}
	if report.sandbox.mode != "off" {
		t.Fatalf("report left sandbox.mode = %q, want off", report.sandbox.mode)
	}
}
