package blocker

import (
	"testing"
	"time"
)

func TestStateIsZero(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "zero value", state: State{}, want: true},
		{name: "kind set", state: State{Kind: KindWorktreeRepair}, want: false},
		{name: "actor set", state: State{Actor: ActorWorkflow}, want: false},
		{name: "code set", state: State{Code: "disk_space"}, want: false},
		{name: "next action set", state: State{NextAction: "repair_worktree"}, want: false},
		{name: "retry after set", state: State{RetryAfter: new(time.Unix(0, 0))}, want: false},
		{name: "exhausted set", state: State{Exhausted: true}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAllowsHumanRequired_OnlyOperatorFacingKinds is the property test named
// in the task's own test approach: every Kind constant is exercised, and only
// the operator-facing kinds (operator_decision, credential_required,
// policy_approval, dependency_scope_unmet) may ever authorize a
// human-required transition. Any new Kind added to the package must be
// explicitly classified here — an unlisted/typo'd kind defaults to "not
// allowed", which is the safe direction (see
// TestAllowsHumanRequired_UnknownKindDenied).
func TestAllowsHumanRequired_OnlyOperatorFacingKinds(t *testing.T) {
	allKinds := []Kind{
		KindOperatorDecision,
		KindCredentialRequired,
		KindPolicyApproval,
		KindWorktreeRepair,
		KindReviewFixExhausted,
		KindTriageRetryExhausted,
		KindDependencyScopeUnmet,
	}
	humanAllowed := map[Kind]bool{
		KindOperatorDecision:     true,
		KindCredentialRequired:   true,
		KindPolicyApproval:       true,
		KindDependencyScopeUnmet: true,
	}
	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			want := humanAllowed[kind]
			if got := AllowsHumanRequired(kind); got != want {
				t.Errorf("AllowsHumanRequired(%q) = %v, want %v", kind, got, want)
			}
		})
	}
}

func TestAllowsHumanRequired_UnknownKindDenied(t *testing.T) {
	if AllowsHumanRequired(Kind("some_future_machine_kind")) {
		t.Error("an unrecognized kind must default to denying human-required, not allowing it")
	}
	if AllowsHumanRequired("") {
		t.Error("the empty kind must not allow human-required")
	}
}

func TestValidateStatus(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		state   State
		wantErr bool
	}{
		{
			name:   "zero state never blocks any status",
			status: "human-required",
			state:  State{},
		},
		{
			name:   "zero state never blocks blocked either",
			status: "blocked",
			state:  State{},
		},
		{
			name:   "operator_decision may reach human-required",
			status: "human-required",
			state:  State{Kind: KindOperatorDecision},
		},
		{
			name:   "credential_required may reach human-required",
			status: "human-required",
			state:  State{Kind: KindCredentialRequired},
		},
		{
			name:   "policy_approval may reach human-required",
			status: "human-required",
			state:  State{Kind: KindPolicyApproval},
		},
		{
			name:   "dependency_scope_unmet may reach human-required",
			status: "human-required",
			state:  State{Kind: KindDependencyScopeUnmet},
		},
		{
			name:    "worktree_repair must never reach human-required",
			status:  "human-required",
			state:   State{Kind: KindWorktreeRepair},
			wantErr: true,
		},
		{
			name:    "review_fix_exhausted must never reach human-required",
			status:  "human-required",
			state:   State{Kind: KindReviewFixExhausted},
			wantErr: true,
		},
		{
			name:    "triage_retry_exhausted must never reach human-required",
			status:  "human-required",
			state:   State{Kind: KindTriageRetryExhausted},
			wantErr: true,
		},
		{
			name:   "a machine kind is fine at blocked",
			status: "blocked",
			state:  State{Kind: KindWorktreeRepair},
		},
		{
			name:   "a machine kind is fine at any non-human-required status",
			status: "in-progress",
			state:  State{Kind: KindWorktreeRepair},
		},
		{
			name:   "an operator kind is also fine to persist at blocked",
			status: "blocked",
			state:  State{Kind: KindOperatorDecision},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStatus(tc.status, tc.state)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateStatus(%q, %+v) error = %v, wantErr %v", tc.status, tc.state, err, tc.wantErr)
			}
		})
	}
}

// TestValidateStatus_IllegalTransitionProperty is the property test named in
// the task's own test approach: for every (kind, status) pair, ValidateStatus
// must reject the pairing if and only if status is human-required and the
// kind is not one of the three operator-facing kinds. This is the invariant
// the whole refactor exists to guarantee — a regression here means a
// machine-recoverable blocker could falsely reach human-required again.
func TestValidateStatus_IllegalTransitionProperty(t *testing.T) {
	allKinds := []Kind{
		KindOperatorDecision,
		KindCredentialRequired,
		KindPolicyApproval,
		KindWorktreeRepair,
		KindReviewFixExhausted,
		KindTriageRetryExhausted,
		KindDependencyScopeUnmet,
	}
	statuses := []string{
		"new", "todo", "in-progress", "blocked", "human-required", "done", "cancelled",
	}
	for _, kind := range allKinds {
		for _, status := range statuses {
			t.Run(string(kind)+"/"+status, func(t *testing.T) {
				err := ValidateStatus(status, State{Kind: kind})
				wantErr := status == "human-required" && !AllowsHumanRequired(kind)
				if (err != nil) != wantErr {
					t.Errorf("ValidateStatus(%q, kind=%q) error = %v, wantErr %v", status, kind, err, wantErr)
				}
			})
		}
	}
}
