package autonomy

import "testing"

func TestFailureOwnerHumanRequiredEligibility(t *testing.T) {
	t.Parallel()
	allowed := map[FailureOwner]bool{
		FailureOwnerOperatorAuthority: true,
		FailureOwnerOperatorDecision:  true,
		FailureOwnerSpecification:     true,
		FailureOwnerPolicy:            true,
	}
	owners := []FailureOwner{
		FailureOwnerUnknown, FailureOwnerMachine, FailureOwnerExternalTransient,
		FailureOwnerOperatorAuthority, FailureOwnerOperatorDecision,
		FailureOwnerSpecification, FailureOwnerPolicy,
	}
	for _, owner := range owners {
		if got := owner.AllowsHumanRequired(); got != allowed[owner] {
			t.Errorf("FailureOwner(%q).AllowsHumanRequired() = %v, want %v", owner, got, allowed[owner])
		}
	}
}

func TestLegacyReasonIsConservativelyIneligible(t *testing.T) {
	t.Parallel()
	r := LegacyReason("old prose")
	if r.Owner != FailureOwnerUnknown || r.Provenance != ProvenanceLegacy {
		t.Fatalf("LegacyReason() = %#v", r)
	}
	if err := r.ValidateHumanRequired(); err == nil {
		t.Fatal("legacy reason unexpectedly eligible for human-required")
	}
}

func TestAllCapabilityRequirementsValidate(t *testing.T) {
	t.Parallel()
	for _, capability := range AllCapabilities() {
		r := CapabilityRequirement{Capability: capability, Action: "test", Scope: "task"}
		if err := r.Validate(); err != nil {
			t.Errorf("requirement for %q: %v", capability, err)
		}
	}
}
