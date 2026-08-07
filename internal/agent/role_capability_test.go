package agent

import "testing"

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
