package agent

import (
	"testing"

	"github.com/Automaat/sybra/internal/autonomy"
)

func TestEveryRoleDeclaresValidCapabilities(t *testing.T) {
	t.Parallel()
	for _, role := range AllRoles() {
		requirements := role.CapabilityRequirements("dispatch")
		if len(requirements) == 0 {
			t.Fatalf("role %q declares no capabilities", role)
		}
		seen := map[string]bool{}
		for _, requirement := range requirements {
			if err := requirement.Validate(); err != nil {
				t.Errorf("role %q: %v", role, err)
			}
			key := string(requirement.Capability)
			if seen[key] {
				t.Errorf("role %q declares capability %q twice", role, key)
			}
			seen[key] = true
		}
	}
}

func TestVerifierCapabilityContractDoesNotGrantAuthoritativeSourceWrite(t *testing.T) {
	t.Parallel()
	for _, role := range []Role{RoleReview, RolePlan, RolePlanCritic, RoleEval, RoleTestRunner} {
		hasScratchWrite := false
		for _, requirement := range role.CapabilityRequirements("verify") {
			if requirement.Capability == "source_write" {
				t.Errorf("verifier role %q received source_write", role)
			}
			if requirement.Capability == "scratch_write" {
				hasScratchWrite = true
			}
		}
		if !hasScratchWrite {
			t.Errorf("verifier role %q did not receive scratch_write", role)
		}
	}
}

func TestCheckoutCapabilitiesOnlyForCheckoutRoles(t *testing.T) {
	t.Parallel()
	for _, role := range AllRoles() {
		want := role.AuthorsCode() || role.JudgesWithoutWriting() || role == RoleTestRunner
		gotObjectStore := false
		gotCheckoutHealth := false
		for _, requirement := range role.CapabilityRequirements("dispatch") {
			if requirement.Capability == autonomy.CapabilityObjectStore {
				gotObjectStore = true
			}
			if requirement.Capability == autonomy.CapabilityCheckoutHealth {
				gotCheckoutHealth = true
			}
		}
		if gotObjectStore != want || gotCheckoutHealth != want {
			t.Errorf("role %q checkout capabilities = object_store:%v checkout_health:%v, want both %v", role, gotObjectStore, gotCheckoutHealth, want)
		}
	}
}
