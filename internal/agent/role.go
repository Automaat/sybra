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

// IsVerifier reports whether the role independently checks another agent's
// work (review, test-runner, eval). These roles must not have their turn
// budget auto-bumped: a verifier stuck in a fan-out loop should escalate to a
// human promptly rather than being handed progressively larger turn budgets.
func (r Role) IsVerifier() bool {
	switch r {
	case RoleReview, RoleTestRunner, RoleEval:
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

// SupportsHeadlessSteer reports whether headless runs of this role should be
// launched with the stdin/stream-json steerable transport
// (RunConfig.HeadlessSteerable). Steering exists so a human watching a live
// run from the GUI can send follow-up guidance; verifier/system roles
// (review, test-runner, eval, triage, plan-critic, human-review) and
// fix-review are dispatched unattended by pollers/watchers with
// Mode hardcoded to "headless" independent of the originating task's own
// AgentMode (see internal/sybra/review/inbound.go, app_human_review.go) —
// nothing ever writes a steer message to them. Forcing the steerable
// transport onto that unattended dispatch produced the stdin deadlock in
// #1825: the process sat waiting on a FIFO no caller intended to feed.
// Excluding these roles falls back to the plain one-shot `-p <prompt>`
// invocation, which has no stdin dependency to hang on.
func (r Role) SupportsHeadlessSteer() bool {
	if r.IsVerifier() || r.IsSystem() {
		return false
	}
	return r != RoleFixReview
}

// ParseRoleFromName extracts the Role from a prefixed agent name.
// The bool reports whether the name carried a known role prefix.
func ParseRoleFromName(name string) (Role, bool) {
	prefix, _, ok := strings.Cut(name, ":")
	if !ok {
		return RoleImplementation, false
	}
	r := Role(prefix)
	switch r {
	case RoleTriage, RolePlan, RolePlanCritic, RoleEval, RolePRFix, RoleReview, RoleFixReview, RoleTestRunner, RoleImplementation, RoleHumanReview:
		return r, true
	default:
		return RoleImplementation, false
	}
}

// RoleFromName extracts the Role from a prefixed agent name.
// Returns RoleImplementation for names without a known prefix.
func RoleFromName(name string) Role {
	r, _ := ParseRoleFromName(name)
	return r
}

// DefaultReasoningEffort returns the built-in per-role reasoning-effort
// baseline used when neither an experiment assignment nor the task itself
// pins a level (see RunConfig.ReasoningEffort). System/verifier roles that
// mostly classify or check pre-existing work default to "low"; the
// code-authoring roles that most benefit from deeper reasoning default to
// "high". Everything else falls back to DefaultReasoningEffort ("medium")
// via the empty return.
func (r Role) DefaultReasoningEffort() string {
	switch r {
	case RoleTriage, RoleEval, RolePlanCritic, RoleHumanReview:
		return "low"
	case RoleImplementation, RoleFixReview, RolePRFix:
		return "high"
	default:
		return ""
	}
}
