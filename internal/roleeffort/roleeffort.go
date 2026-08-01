// Package roleeffort owns the built-in per-role reasoning-effort baseline.
//
// It is a dependency-free leaf so that both internal/agent (which resolves the
// level for a dispatch) and internal/abtest (which must know what an omitted
// variant reasoning_effort resolves to) can share one table. internal/abtest
// cannot reach the level through internal/agent: agent transitively depends on
// internal/config, which depends on abtest.
package roleeffort

// Global is the level used when neither the caller, the operator config, nor
// the role itself pins one. Mirrored by agent.DefaultReasoningEffort.
const Global = "medium"

// ForRole returns the built-in baseline for role, or "" when the role carries
// no opinion and should inherit Global. Role names are the string values of
// agent.Role; TestForRole_CoversEveryKnownRole in internal/agent guards the
// two lists against drift.
//
// The ladder spends reasoning depth where a mistake is most expensive to
// unwind: planning picks the approach every later stage inherits, and review
// is the gate that decides whether a change lands. Roles that mostly classify
// or check pre-existing work sit at "low". Implementation is deliberately not
// "high" — it executes an already-approved plan, and it is the
// highest-volume role by a wide margin.
func ForRole(role string) string {
	switch role {
	case "triage", "eval", "plan-critic", "human-review", "monitor":
		return "low"
	case "plan", "review", "fix-review", "pr-fix", "test-fix":
		return "high"
	default:
		return ""
	}
}

// Resolve returns the effective baseline for role, falling back to Global for
// roles ForRole has no opinion on. Never returns "".
func Resolve(role string) string {
	if effort := ForRole(role); effort != "" {
		return effort
	}
	return Global
}
