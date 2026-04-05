package agent

import "strings"

// Role identifies the purpose of an agent run.
type Role string

const (
	RoleTriage         Role = "triage"
	RolePlan           Role = "plan"
	RoleEval           Role = "eval"
	RolePRFix          Role = "pr-fix"
	RoleReview         Role = "review"
	RoleImplementation Role = "implementation"
)

// AgentName returns the prefixed name used when launching an agent
// (e.g. "triage:My Task Title").
func (r Role) AgentName(title string) string { return string(r) + ":" + title }

// IsSystem returns true for roles whose agents should not trigger
// user-facing notifications (triage, eval).
func (r Role) IsSystem() bool { return r == RoleTriage || r == RoleEval }

// RoleFromName extracts the Role from a prefixed agent name.
// Returns RoleImplementation for names without a known prefix.
func RoleFromName(name string) Role {
	prefix, _, ok := strings.Cut(name, ":")
	if !ok {
		return RoleImplementation
	}
	r := Role(prefix)
	switch r {
	case RoleTriage, RolePlan, RoleEval, RolePRFix, RoleReview:
		return r
	default:
		return RoleImplementation
	}
}
