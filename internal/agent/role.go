package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/roleeffort"
)

// Role identifies the purpose of an agent run.
type Role string

const (
	RoleTriage         Role = "triage"
	RolePlan           Role = "plan"
	RolePlanCritic     Role = "plan-critic"
	RoleEval           Role = "eval"
	RoleLoop           Role = "loop"
	RoleMonitor        Role = "monitor"
	RoleOrchestrator   Role = "orchestrator"
	RolePRFix          Role = "pr-fix"
	RoleReview         Role = "review"
	RoleFixReview      Role = "fix-review"
	RoleTestRunner     Role = "test-runner"
	RoleImplementation Role = "implementation"
	RoleHumanReview    Role = "human-review"
	// RoleTestFix is pr-fix's bounded follow-up: given the specific failing
	// tests pr-fix already found (PRFixVerdictVar's sibling failing-tests
	// var), fix only those and nothing else. Dispatched at most once per
	// pr-fix run — see pr-fix.yaml's test_fix/route_test_fix_result steps.
	RoleTestFix Role = "test-fix"
)

// allRoles is the single source of truth for the role set. Package-level so
// IsKnown — called on every dispatch through ResolveRunRole — stays
// allocation-free; AllRoles hands out a copy so no caller can mutate it.
var allRoles = []Role{
	RoleTriage, RolePlan, RolePlanCritic, RoleEval, RoleLoop, RoleMonitor,
	RoleOrchestrator, RolePRFix, RoleReview, RoleFixReview, RoleTestRunner,
	RoleImplementation, RoleHumanReview, RoleTestFix,
}

// AllRoles lists every known Role. IsKnown is derived from the same list, so
// adding a Role constant to it is what makes the role dispatchable — and what
// makes the reasoning-effort coverage test notice it.
func AllRoles() []Role {
	return slices.Clone(allRoles)
}

func (r Role) IsKnown() bool {
	return slices.Contains(allRoles, r)
}

// AgentName returns the prefixed name used when launching an agent
// (e.g. "triage:My Task Title").
func (r Role) AgentName(title string) string { return string(r) + ":" + title }

// JudgesWithoutWriting reports whether the role inspects a worktree it must
// not modify. These roles reuse the *same* per-task worktree as the
// implementer, so a writable tree lets a reviewer quietly fix what it was
// asked to judge — and a tool allowlist cannot express the restriction,
// because every role has Bash and `sed -i`, a shell redirect or `git checkout`
// reach the same files. The restriction is therefore enforced at the OS level
// via RunConfig.ReadOnlyDir, which also covers grandchildren.
//
// test-runner is deliberately excluded: building and running tests legitimately
// writes into the tree, so it stays governed by the diff-based tamper gate.
// human-review is excluded too: it is a recovery author that fixes, commits,
// and pushes after a task is already blocked, rather than an independent
// judge of an in-flight implementation.
func (r Role) JudgesWithoutWriting() bool {
	switch r {
	case RoleReview, RolePlan, RolePlanCritic, RoleEval:
		return true
	default:
		return false
	}
}

// DiagnosesBlockedTask reports whether the role is dispatched *because* a
// task is in a state that otherwise means "no agent should be running" —
// human-required, or terminal.
//
// Both status reapers (watchdog.reapTaskAgentForStatus and
// App.releaseTaskAgents) stop agents whose task reaches such a status. These
// roles are the exception: killing them for the very condition they were sent
// to resolve leaves the task stuck and the detector re-dispatching on its next
// cycle, which is a livelock that costs a full agent run each pass.
func (r Role) DiagnosesBlockedTask() bool {
	switch r {
	case RoleHumanReview, RoleMonitor:
		return true
	default:
		return false
	}
}

// AuthorsCode reports whether the role produces code commits and may therefore
// be primed with the task's NOTES.md working memory. Only these roles inherit
// the scratchpad: independent verifier roles (review, test-runner, eval) must
// NOT, or the implementer's notes could bias an adversarial check — and because
// NOTES.md is git-excluded, that bias is invisible to the diff-based
// reward-hacking gates (detect_tampering / verify_checks). Empty name maps to
// implementation, matching RoleFromName.
func (r Role) AuthorsCode() bool {
	switch r {
	case RoleImplementation, RoleFixReview, RolePRFix, RoleTestFix, RoleHumanReview, "":
		return true
	default:
		return false
	}
}

// CapabilityRequirements is the single declaration API used by admission to
// describe what a role/action needs before provider tokens are spent. Action
// is persisted in certificates so two actions performed by the same role do
// not become indistinguishable evidence.
func (r Role) CapabilityRequirements(action string) []autonomy.CapabilityRequirement {
	if strings.TrimSpace(action) == "" {
		action = string(r)
	}
	requirements := []autonomy.CapabilityRequirement{
		{Capability: autonomy.CapabilitySourceRead, Action: action, Scope: "task", Repairable: true},
		{Capability: autonomy.CapabilityObjectStore, Action: action, Scope: "project", Repairable: true},
		{Capability: autonomy.CapabilityCheckoutHealth, Action: action, Scope: "task", Repairable: true},
		{Capability: autonomy.CapabilitySandboxMechanism, Action: action, Scope: "host"},
		{Capability: autonomy.CapabilityProviderCapacity, Action: action, Scope: "provider"},
	}
	if r.AuthorsCode() {
		requirements = append(requirements,
			autonomy.CapabilityRequirement{Capability: autonomy.CapabilitySourceWrite, Action: action, Scope: "task", Repairable: true},
			autonomy.CapabilityRequirement{Capability: autonomy.CapabilityGitAdminWrite, Action: action, Scope: "task", Repairable: true},
			autonomy.CapabilityRequirement{Capability: autonomy.CapabilitySigning, Action: action, Scope: "host"},
		)
	}
	if r == RoleTriage || r == RoleMonitor || r == RoleOrchestrator || r == RoleHumanReview || r.AuthorsCode() {
		requirements = append(requirements,
			autonomy.CapabilityRequirement{Capability: autonomy.CapabilityTaskMutation, Action: action, Scope: "task"},
		)
	}
	if r == RoleReview || r == RoleFixReview || r == RolePRFix || r == RoleHumanReview || r == RoleTestFix {
		requirements = append(requirements,
			autonomy.CapabilityRequirement{Capability: autonomy.CapabilityNetworkGitHub, Action: action, Scope: "project"},
		)
	}
	return requirements
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
// user-facing notifications (triage, eval, plan-critic, human-review,
// monitor, loop, orchestrator).
func (r Role) IsSystem() bool {
	switch r {
	case RoleTriage, RoleEval, RolePlanCritic, RoleHumanReview, RoleMonitor, RoleLoop, RoleOrchestrator:
		return true
	default:
		return false
	}
}

// WorkloadClass partitions dispatch capacity into a small set of pools so one
// kind of work (e.g. a retry storm of system/monitor runs) cannot starve
// another (e.g. new implementation work). See internal/agent/capacity.go for
// the reserve-with-borrowing admission rule that consumes this.
type WorkloadClass string

const (
	// ClassImplementation covers new work that has not yet landed: fresh
	// implementation runs and their planning step. Deterministic recovery
	// (restarting a stalled implementation run) also folds in here for v1 —
	// it is not yet a distinct class (see task #2463 subissues).
	ClassImplementation WorkloadClass = "implementation"
	// ClassCompletion covers work that lands or verifies an already-started
	// PR: review, fix-review, pr-fix, test-runner, test-fix.
	ClassCompletion WorkloadClass = "completion"
	// ClassSystem covers unattended system/monitor roles (Role.IsSystem()).
	ClassSystem WorkloadClass = "system"
)

// AllWorkloadClasses lists every known WorkloadClass. Fixed and small by
// design — admitClass and the Manager's per-class accounting maps iterate
// this instead of tracking an open-ended set of keys.
func AllWorkloadClasses() []WorkloadClass {
	return []WorkloadClass{ClassImplementation, ClassCompletion, ClassSystem}
}

// ParseClassReservations converts the config-level agent.class_reservations
// map (string keys, so internal/config never needs to import this package)
// into the WorkloadClass-keyed map ManagerRuntimeConfig.ClassReservations
// expects. Unknown keys are dropped defensively — config validation
// (internal/config.validateClassReservations) already rejects them at
// startup, so this only guards against a caller that skipped validation
// (e.g. a test building config by hand).
func ParseClassReservations(in map[string]int) map[WorkloadClass]int {
	out := make(map[WorkloadClass]int, len(in))
	for k, v := range in {
		c := WorkloadClass(k)
		if !slices.Contains(AllWorkloadClasses(), c) {
			continue
		}
		out[c] = v
	}
	return out
}

// WorkloadClass maps a role to the capacity pool it draws from. Every known
// Role resolves to exactly one class; an unknown/empty role falls back to
// ClassImplementation, matching RoleFromName's own fallback semantics.
func (r Role) WorkloadClass() WorkloadClass {
	switch r {
	case RoleTriage, RoleEval, RolePlanCritic, RoleHumanReview, RoleMonitor, RoleLoop, RoleOrchestrator:
		return ClassSystem
	case RolePRFix, RoleReview, RoleFixReview, RoleTestRunner, RoleTestFix:
		return ClassCompletion
	case RoleImplementation, RolePlan:
		return ClassImplementation
	default:
		return ClassImplementation
	}
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
//
// RoleOrchestrator is the one system role exempted from that exclusion: it
// is the long-lived brain session a human actively drives via SendMessage
// (unlike the other system roles, which are dispatched unattended), so it
// needs the steerable transport despite being IsSystem().
func (r Role) SupportsHeadlessSteer() bool {
	if r == RoleOrchestrator {
		return true
	}
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
	if r.IsKnown() {
		return r, true
	}
	return RoleImplementation, false
}

// RoleFromName extracts the Role from a prefixed agent name.
// Returns RoleImplementation for names without a known prefix.
// Legacy-only fallback: new dispatch paths should set RunConfig.Role.
func RoleFromName(name string) Role {
	r, _ := ParseRoleFromName(name)
	return r
}

// ResolveRunRole returns the effective role for a newly dispatched run.
// Explicit cfg.Role wins. Prefix parsing remains only as a compatibility path
// for callers that still build Name-only configs.
func ResolveRunRole(role Role, name string) (Role, error) {
	if role != "" {
		if role.IsKnown() {
			return role, nil
		}
		return "", errUnknownRole(role)
	}
	if !strings.Contains(name, ":") {
		return RoleImplementation, nil
	}
	if resolved, ok := ParseRoleFromName(name); ok {
		return resolved, nil
	}
	prefix, _, _ := strings.Cut(name, ":")
	if !looksLikeRoleToken(prefix) {
		return RoleImplementation, nil
	}
	return "", errUnknownRolePrefix(name)
}

func looksLikeRoleToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func errUnknownRole(role Role) error {
	return fmt.Errorf("unknown agent role %q", role)
}

func errUnknownRolePrefix(name string) error {
	prefix, _, _ := strings.Cut(name, ":")
	return fmt.Errorf("unknown agent role prefix %q in %q", prefix, name)
}

// DefaultReasoningEffort returns the built-in per-role reasoning-effort
// baseline used when neither an experiment assignment, the operator's
// agent.role_effort map, nor the task itself pins a level (see
// RunConfig.ReasoningEffort). Roles with no opinion return "" and fall back to
// DefaultReasoningEffort ("medium").
//
// The table lives in internal/roleeffort so internal/abtest can share it
// without importing this package (that direction cycles through
// internal/config).
func (r Role) DefaultReasoningEffort() string {
	return roleeffort.ForRole(string(r))
}
