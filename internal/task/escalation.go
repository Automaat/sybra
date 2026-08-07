package task

import (
	"errors"
	"fmt"

	"github.com/Automaat/sybra/internal/autonomy"
)

// Typed escalation constructors keep status writers concise while forcing an
// explicit ownership decision. These constructors describe observations made
// by deterministic control-plane code. Direct UI/CLI decisions use
// OperatorDecisionEvidence instead.
func MachineFailure(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerMachine, autonomy.ProvenanceControlPlane, message)
}

func ControlPlaneFailure(code string, owner autonomy.FailureOwner, message string) *autonomy.EscalationReason {
	return escalation(code, owner, autonomy.ProvenanceControlPlane, message)
}

func ExternalFailure(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerExternalTransient, autonomy.ProvenanceControlPlane, message)
}

func OperatorAuthorityRequired(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerOperatorAuthority, autonomy.ProvenanceControlPlane, message)
}

func OperatorDecisionRequired(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerOperatorDecision, autonomy.ProvenanceControlPlane, message)
}

func SpecificationRequired(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerSpecification, autonomy.ProvenanceControlPlane, message)
}

func PolicyRequired(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerPolicy, autonomy.ProvenanceControlPlane, message)
}

func OperatorDecisionEvidence(code, message string) *autonomy.EscalationReason {
	return escalation(code, autonomy.FailureOwnerOperatorDecision, autonomy.ProvenanceOperator, message)
}

func HumanRequiredOutcome() *autonomy.Outcome {
	return autonomyOutcomePtr(autonomy.OutcomeHumanRequired)
}
func QuarantinedOutcome() *autonomy.Outcome { return autonomyOutcomePtr(autonomy.OutcomeQuarantined) }
func RetriedOutcome() *autonomy.Outcome     { return autonomyOutcomePtr(autonomy.OutcomeRetried) }
func WaitingExternalOutcome() *autonomy.Outcome {
	return autonomyOutcomePtr(autonomy.OutcomeWaitingExternal)
}

func escalation(code string, owner autonomy.FailureOwner, provenance autonomy.EvidenceProvenance, message string) *autonomy.EscalationReason {
	reason := autonomy.NewEscalation(code, owner, provenance, message)
	return &reason
}

func autonomyOutcomePtr(outcome autonomy.Outcome) *autonomy.Outcome { return new(outcome) }

// validateHumanRequiredTransition is deliberately enforced at the Manager
// transition boundary rather than in Store. Store remains able to load and
// repair legacy records, while every sanctioned production status writer
// must prove that a human can actually resolve the escalation.
func validateHumanRequiredTransition(from, to Status, extra Update) error {
	if to != StatusHumanRequired {
		return nil
	}
	if extra.AutonomyOutcome != nil && *extra.AutonomyOutcome != autonomy.OutcomeHumanRequired {
		return errors.New("human-required status requires autonomy outcome human_required")
	}
	if from == StatusHumanRequired && extra.Escalation == nil {
		return nil
	}
	if extra.Escalation == nil {
		return errors.New("transition to human-required requires a typed escalation reason")
	}
	if err := extra.Escalation.ValidateHumanRequired(); err != nil {
		return fmt.Errorf("transition to human-required: %w", err)
	}
	if extra.AutonomyOutcome == nil || *extra.AutonomyOutcome != autonomy.OutcomeHumanRequired {
		return errors.New("transition to human-required requires autonomy outcome human_required")
	}
	return nil
}

func validateTypedAutonomyEvidence(extra Update) error {
	if extra.AutonomyOutcome != nil && !extra.AutonomyOutcome.IsKnown() {
		return errors.New("autonomy outcome must be known")
	}
	if extra.Escalation == nil {
		return nil
	}
	if err := extra.Escalation.Validate(); err != nil {
		return fmt.Errorf("typed escalation: %w", err)
	}
	if extra.AutonomyOutcome == nil {
		return errors.New("typed escalation requires a known autonomy outcome")
	}
	return nil
}
