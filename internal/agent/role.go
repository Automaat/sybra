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
	RoleTestRunner     Role = "test-runner"
	RoleImplementation Role = "implementation"
	RoleHumanReview    Role = "human-review"
)

// AgentName returns the prefixed name used when launching an agent
// (e.g. "triage:My Task Title").
func (r Role) AgentName(title string) string { return string(r) + ":" + title }

// AuthorsCode reports whether the role produces code commits and may therefore
// be primed with the task's NOTES.md working memory. Only these roles inherit
// the scratchpad: independent verifier roles (review, test-runner, eval) must
// NOT, or the implementer's notes could bias an adversarial check — and because
// NOTES.md is git-excluded, that bias is invisible to the diff-based
// reward-hacking gates (detect_tampering / verify_checks). Empty name maps to
// implementation, matching RoleFromName.
func (r Role) AuthorsCode() bool {
	switch r {
	case RoleImplementation, RoleFixReview, RolePRFix, "":
		return true
	default:
		return false
	}
}

// IsSystem returns true for roles whose agents should not trigger
// user-facing notifications (triage, eval, plan-critic, human-review).
func (r Role) IsSystem() bool {
	return r == RoleTriage || r == RoleEval || r == RolePlanCritic || r == RoleHumanReview
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
	case RoleTriage, RolePlan, RolePlanCritic, RoleEval, RolePRFix, RoleReview, RoleFixReview, RoleTestRunner, RoleHumanReview:
		return r
	default:
		return RoleImplementation
	}
}
