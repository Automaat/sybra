package agent

import (
	"fmt"

	"github.com/Automaat/sybra/internal/config"
)

// SandboxPostureObservation separates mechanism availability from actual
// containment. In particular report mode may validate a mechanism but is
// never evidence that the provider process will be contained.
type SandboxPostureObservation struct {
	Mode      string
	Mechanism string
	Available bool
	Contained bool
	Evidence  string
}

// hostSandboxMechanismErr is a variable so a test can present a host whose
// wrapper cannot build a sandbox on a platform where it can.
var hostSandboxMechanismErr = sandboxMechanismErr

// sandboxExecAvailable reports whether this host can actually build the
// process sandbox. It is the boolean view of the mechanism error, which
// carries the reason a caller has to show an operator.
func sandboxExecAvailable() bool { return hostSandboxMechanismErr() == nil }

// ProbeSandboxPosture validates the host mechanism and embedded profile
// before dispatch, without spawning a provider process.
func ProbeSandboxPosture(rawMode string) (SandboxPostureObservation, error) {
	mode, err := config.NormalizeSandboxMode(rawMode)
	if err != nil {
		return SandboxPostureObservation{}, err
	}
	observation := SandboxPostureObservation{Mode: mode, Mechanism: sandboxWrapperName()}
	if mode == "off" {
		observation.Available = true
		observation.Evidence = "sandbox posture is explicitly off"
		return observation, nil
	}
	if mechanismErr := hostSandboxMechanismErr(); mechanismErr != nil {
		observation.Evidence = sandboxWrapperName() + " unavailable: " + mechanismErr.Error()
		if mode == "report" {
			// Report is observational only and must never block a run or claim
			// containment, even when the host wrapper is absent.
			observation.Available = true
			return observation, nil
		}
		return observation, fmt.Errorf("enforce sandbox mode requires %s: %w", sandboxWrapperName(), mechanismErr)
	}
	observation.Available = true
	if mode == "report" {
		observation.Evidence = sandboxWrapperName() + " available; report mode does not wrap the process"
		return observation, nil
	}
	profile, profileErr := materializeSandboxProfile()
	if profileErr != nil {
		observation.Available = false
		observation.Evidence = "sandbox profile unavailable"
		return observation, profileErr
	}
	observation.Contained = true
	observation.Evidence = sandboxWrapperName() + " profile ready at " + profile
	return observation, nil
}
