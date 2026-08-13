package agent

import (
	"errors"
	"strings"
	"testing"
)

// A host whose wrapper is present but cannot build a sandbox must refuse to
// certify enforce, and the refusal must carry the host reason — an operator
// reading "unavailable" alone cannot tell a missing binary from a kernel that
// denies the namespace.
func TestProbeSandboxPostureCarriesMechanismReason(t *testing.T) {
	blocked := errors.New("bwrap cannot create a user namespace on this host: setting up uid map: Permission denied")
	original := hostSandboxMechanismErr
	hostSandboxMechanismErr = func() error { return blocked }
	t.Cleanup(func() { hostSandboxMechanismErr = original })

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
