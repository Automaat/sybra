package agent

import "strings"

// Role identifies the purpose of an agent run.
type Role string

const (
	RoleTriage         Role = "triage"
	RolePlan           Role = "plan"
	RolePlanCritic     Role = "plan-critic"
	RoleEval           Role = "eval"
	RolePRFix          Role = "pr-fix"
	RoleReview         Role = "review"
	RoleFixReview      Role = "fix-review"
	RoleTestPlan       Role = "test-plan"
	RoleTestRunner     Role = "test-runner"
	RoleImplementation Role = "implementation"
)

// AgentName returns the prefixed name used when launching an agent
// (e.g. "triage:My Task Title").
func (r Role) AgentName(title string) string { return string(r) + ":" + title }

// IsSystem returns true for roles whose agents should not trigger
// user-facing notifications (triage, eval, plan-critic).
func (r Role) IsSystem() bool {
	return r == RoleTriage || r == RoleEval || r == RolePlanCritic
}

// RoleFromName extracts the Role from a prefixed agent name.
// Returns RoleImplementation for names without a known prefix.
func RoleFromName(name string) Role {
	prefix, _, ok := strings.Cut(name, ":")
	if !ok {
		return RoleImplementation
	}
	r := Role(prefix)
	switch r {
	case RoleTriage, RolePlan, RolePlanCritic, RoleEval, RolePRFix, RoleReview, RoleFixReview, RoleTestPlan, RoleTestRunner:
		return r
	default:
		return RoleImplementation
	}
}
