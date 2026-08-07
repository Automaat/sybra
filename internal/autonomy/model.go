// Package autonomy defines the stable control-plane vocabulary shared by
// dispatch, workflow, recovery, monitoring, audit, and evaluation.
package autonomy

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Capability is one independently certifiable property of a run environment.
type Capability string

const (
	CapabilitySourceRead       Capability = "source_read"
	CapabilitySourceWrite      Capability = "source_write"
	CapabilityScratchWrite     Capability = "scratch_write"
	CapabilityGitAdminWrite    Capability = "git_admin_write"
	CapabilityCheckoutHealth   Capability = "checkout_health"
	CapabilityObjectStore      Capability = "object_store_health"
	CapabilitySigning          Capability = "signing"
	CapabilityTaskMutation     Capability = "task_mutation"
	CapabilitySandboxMechanism Capability = "sandbox_mechanism"
	CapabilityProviderCapacity Capability = "provider_capacity"
	CapabilityNetworkGitHub    Capability = "network_github"
)

var allCapabilities = []Capability{
	CapabilitySourceRead, CapabilitySourceWrite, CapabilityScratchWrite, CapabilityGitAdminWrite,
	CapabilityCheckoutHealth, CapabilityObjectStore, CapabilitySigning, CapabilityTaskMutation,
	CapabilitySandboxMechanism, CapabilityProviderCapacity, CapabilityNetworkGitHub,
}

func AllCapabilities() []Capability { return slices.Clone(allCapabilities) }

func (c Capability) IsKnown() bool { return slices.Contains(allCapabilities, c) }

// CapabilityRequirement declares why an action needs a capability and the
// scope in which it must be certified. Repairable means the control plane has
// a safe deterministic repair it may attempt before quarantining the scope.
type CapabilityRequirement struct {
	Capability Capability `json:"capability" yaml:"capability"`
	Action     string     `json:"action" yaml:"action"`
	Scope      string     `json:"scope" yaml:"scope"`
	Repairable bool       `json:"repairable" yaml:"repairable"`
}

func (r CapabilityRequirement) Validate() error {
	if !r.Capability.IsKnown() {
		return fmt.Errorf("unknown capability %q", r.Capability)
	}
	if strings.TrimSpace(r.Action) == "" {
		return fmt.Errorf("capability %q: action is required", r.Capability)
	}
	if strings.TrimSpace(r.Scope) == "" {
		return fmt.Errorf("capability %q: scope is required", r.Capability)
	}
	return nil
}

// FailureOwner identifies who can actually resolve a failure. Policy must
// branch on this value, never on display text.
type FailureOwner string

const (
	FailureOwnerUnknown           FailureOwner = "unknown"
	FailureOwnerMachine           FailureOwner = "machine"
	FailureOwnerExternalTransient FailureOwner = "external_transient"
	FailureOwnerOperatorAuthority FailureOwner = "operator_authority"
	FailureOwnerOperatorDecision  FailureOwner = "operator_decision"
	FailureOwnerSpecification     FailureOwner = "specification"
	FailureOwnerPolicy            FailureOwner = "policy"
)

// AllowsHumanRequired is the one eligibility guard for the human-required
// state. Unknown is deliberately denied: old prose is not proof of ownership.
func (o FailureOwner) AllowsHumanRequired() bool {
	switch o {
	case FailureOwnerOperatorAuthority, FailureOwnerOperatorDecision,
		FailureOwnerSpecification, FailureOwnerPolicy:
		return true
	default:
		return false
	}
}

// Outcome is the stable result of one autonomous control-plane decision.
type Outcome string

const (
	OutcomeAdvanced        Outcome = "advanced"
	OutcomeRepaired        Outcome = "repaired"
	OutcomeRetried         Outcome = "retried"
	OutcomeQuarantined     Outcome = "quarantined"
	OutcomeWaitingExternal Outcome = "waiting_external"
	OutcomeHumanRequired   Outcome = "human_required"
)

func (o Outcome) IsKnown() bool {
	switch o {
	case OutcomeAdvanced, OutcomeRepaired, OutcomeRetried, OutcomeQuarantined,
		OutcomeWaitingExternal, OutcomeHumanRequired:
		return true
	default:
		return false
	}
}

// EvidenceProvenance identifies the authority that observed the evidence.
// Detail belongs in scrubbed display text or an artifact; policy only needs
// this stable provenance and the categorical code below.
type EvidenceProvenance string

const (
	ProvenanceLegacy       EvidenceProvenance = "legacy"
	ProvenanceOperator     EvidenceProvenance = "operator"
	ProvenanceControlPlane EvidenceProvenance = "control_plane"
	ProvenanceProvider     EvidenceProvenance = "provider"
	ProvenanceGit          EvidenceProvenance = "git"
	ProvenanceGitHub       EvidenceProvenance = "github"
	ProvenanceFilesystem   EvidenceProvenance = "filesystem"
)

// EscalationReason is the typed, persisted authority for an escalation.
// Message is display-only and must already be scrubbed before persistence.
type EscalationReason struct {
	Code       string             `json:"code" yaml:"code"`
	Owner      FailureOwner       `json:"owner" yaml:"owner"`
	Provenance EvidenceProvenance `json:"provenance" yaml:"provenance"`
	ObservedAt time.Time          `json:"observedAt,omitzero" yaml:"observed_at,omitempty"`
	Message    string             `json:"message,omitempty" yaml:"message,omitempty"`
}

// NewEscalation builds a current typed reason at the observation boundary.
func NewEscalation(code string, owner FailureOwner, provenance EvidenceProvenance, message string) EscalationReason {
	return EscalationReason{
		Code:       strings.TrimSpace(code),
		Owner:      owner,
		Provenance: provenance,
		ObservedAt: time.Now().UTC(),
		Message:    message,
	}
}

func (r EscalationReason) IsZero() bool {
	return r.Code == "" && r.Owner == "" && r.Provenance == "" && r.ObservedAt.IsZero() && r.Message == ""
}

func (r EscalationReason) Validate() error {
	if strings.TrimSpace(r.Code) == "" {
		return fmt.Errorf("escalation reason code is required")
	}
	if r.Owner == "" || r.Owner == FailureOwnerUnknown {
		return fmt.Errorf("escalation reason %q has no classified owner", r.Code)
	}
	if r.Provenance == "" || r.Provenance == ProvenanceLegacy {
		return fmt.Errorf("escalation reason %q has no authoritative evidence provenance", r.Code)
	}
	return nil
}

func (r EscalationReason) ValidateHumanRequired() error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !r.Owner.AllowsHumanRequired() {
		return fmt.Errorf("failure owner %q cannot transition to human-required", r.Owner)
	}
	return nil
}

// LegacyReason is the conservative adapter for records created before typed
// escalation existed. It is intentionally ineligible for human-required.
func LegacyReason(message string) EscalationReason {
	return EscalationReason{
		Code:       "legacy.unknown",
		Owner:      FailureOwnerUnknown,
		Provenance: ProvenanceLegacy,
		Message:    message,
	}
}
