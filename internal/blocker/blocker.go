package blocker

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/taskstatus"
)

// Kind classifies why a task is parked or retrying.
type Kind string

const (
	KindOperatorDecision           Kind = "operator_decision"
	KindCredentialRequired         Kind = "credential_required"
	KindPolicyApproval             Kind = "policy_approval"
	KindTamperDetected             Kind = "tamper_detected"
	KindWorktreeRepair             Kind = "worktree_repair"
	KindReviewFixExhausted         Kind = "review_fix_exhausted"
	KindTriageRetryExhausted       Kind = "triage_retry_exhausted"
	KindWatchdogRateLimitExhausted Kind = "watchdog_rate_limit_exhausted"
	// KindDependencyScopeUnmet marks a task blocked on a depends_on issue
	// whose closure a prior agent/human run explicitly verified did NOT
	// satisfy the scope this task actually needs (e.g. the closing PR only
	// covered part of the referenced issue). Code names the specific
	// depends_on ref the verdict applies to. The umbrella dependency gate
	// (internal/sybra/app_umbrella_gate.go) trusts this over a bare "done"
	// status: it never silently re-releases a child carrying this kind for
	// one of its own depends_on refs — it escalates to human-required
	// instead, so a fresh confirmation is required before the same,
	// already-known-negative implementation cycle repeats (sybra#2637).
	KindDependencyScopeUnmet Kind = "dependency_scope_unmet"
	// KindDependencyConditionUnmet marks a task blocked on a task.DepCondition
	// of kind "note" attached to one of its depends_on refs: the referenced
	// task is Done, but the free-text acceptance note the condition names has
	// not been confirmed by a human. Deliberately distinct from
	// KindDependencyScopeUnmet — that kind marks a reactive verdict a prior
	// agent/human run recorded *after* observing a closed dependency fall
	// short of scope, whereas this kind marks a proactive condition set
	// *before* the dependency ever closed. Keeping them separate means a
	// human clearing one can never be misread as having confirmed the other
	// (internal/sybra/app_umbrella_gate.go's holdUnmetConditions is the sole
	// writer).
	KindDependencyConditionUnmet Kind = "dependency_condition_unmet"
)

// Actor identifies which subsystem authored the blocker.
type Actor string

const (
	ActorWorkflow Actor = "workflow"
	ActorReview   Actor = "review"
)

// State is the structured blocker payload persisted on a task.
type State struct {
	Kind       Kind       `json:"kind,omitempty" yaml:"kind,omitempty"`
	Actor      Actor      `json:"actor,omitempty" yaml:"actor,omitempty"`
	Code       string     `json:"code,omitempty" yaml:"code,omitempty"`
	NextAction string     `json:"nextAction,omitempty" yaml:"next_action,omitempty"`
	RetryAfter *time.Time `json:"retryAfter,omitempty" yaml:"retry_after,omitempty"`
	Exhausted  bool       `json:"exhausted,omitempty" yaml:"exhausted,omitempty"`
}

func (s State) IsZero() bool {
	return s.Kind == "" && s.Actor == "" && s.Code == "" && s.NextAction == "" && s.RetryAfter == nil && !s.Exhausted
}

func AllowsHumanRequired(kind Kind) bool {
	switch kind {
	case KindOperatorDecision, KindCredentialRequired, KindPolicyApproval, KindTamperDetected, KindDependencyScopeUnmet, KindDependencyConditionUnmet:
		return true
	default:
		return false
	}
}

// FailureOwner maps the older blocker vocabulary into the autonomy policy
// vocabulary without consulting display text. Unknown and machine-repair
// blockers remain machine-owned by default.
func FailureOwner(kind Kind) autonomy.FailureOwner {
	switch kind {
	case KindCredentialRequired:
		return autonomy.FailureOwnerOperatorAuthority
	case KindOperatorDecision:
		return autonomy.FailureOwnerOperatorDecision
	case KindPolicyApproval:
		return autonomy.FailureOwnerPolicy
	case KindDependencyScopeUnmet, KindDependencyConditionUnmet:
		return autonomy.FailureOwnerSpecification
	default:
		return autonomy.FailureOwnerMachine
	}
}

func ValidateStatus(status string, state State) error {
	if state.IsZero() {
		return nil
	}
	if status == string(taskstatus.HumanRequired) && !AllowsHumanRequired(state.Kind) {
		return fmt.Errorf("blocker kind %q cannot transition to human-required", state.Kind)
	}
	return nil
}
