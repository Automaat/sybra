package sybra

import "testing"

func TestComputePRPhase(t *testing.T) {
	tests := []struct {
		name string
		sig  prSignals
		want string
	}{
		{
			name: "agent running trumps everything",
			sig:  prSignals{AgentRunning: true, IsDraft: true, ReviewDecision: "APPROVED"},
			want: PRPhaseFixing,
		},
		{
			name: "CI failure → fixing",
			sig:  prSignals{CIStatus: "FAILURE"},
			want: PRPhaseFixing,
		},
		{
			name: "CI failure with pending checks is not yet fixing",
			sig:  prSignals{CIStatus: "FAILURE", HasPendingChecks: true},
			want: PRPhaseBuilding,
		},
		{
			name: "merge conflict → fixing",
			sig:  prSignals{Mergeable: "CONFLICTING"},
			want: PRPhaseFixing,
		},
		{
			name: "changes requested → changes-requested",
			sig:  prSignals{ReviewDecision: "CHANGES_REQUESTED"},
			want: PRPhaseChangesRequested,
		},
		{
			name: "actionable threads → changes-requested",
			sig:  prSignals{UnresolvedCount: 2, ActionableCount: 2},
			want: PRPhaseChangesRequested,
		},
		{
			name: "unresolved but addressed (agent replied) → awaiting-approval, not changes",
			sig:  prSignals{UnresolvedCount: 2, ActionableCount: 0},
			want: PRPhaseAwaitingApproval,
		},
		{
			name: "draft with comments stays draft (comment-fix is skipped on drafts)",
			sig:  prSignals{IsDraft: true, UnresolvedCount: 1, ActionableCount: 1},
			want: PRPhaseDraft,
		},
		{
			name: "draft with CI failure still shows fixing (CI-fix runs on drafts)",
			sig:  prSignals{IsDraft: true, CIStatus: "FAILURE"},
			want: PRPhaseFixing,
		},
		{
			name: "clean draft → draft",
			sig:  prSignals{IsDraft: true},
			want: PRPhaseDraft,
		},
		{
			name: "CI pending → building",
			sig:  prSignals{CIStatus: "PENDING"},
			want: PRPhaseBuilding,
		},
		{
			name: "pending checks flag → building",
			sig:  prSignals{HasPendingChecks: true},
			want: PRPhaseBuilding,
		},
		{
			name: "approved and clean → approved",
			sig:  prSignals{ReviewDecision: "APPROVED", CIStatus: "SUCCESS", Mergeable: "MERGEABLE"},
			want: PRPhaseApproved,
		},
		{
			name: "approved but CI failing → fixing wins",
			sig:  prSignals{ReviewDecision: "APPROVED", CIStatus: "FAILURE"},
			want: PRPhaseFixing,
		},
		{
			name: "clean green not approved → awaiting approval",
			sig:  prSignals{CIStatus: "SUCCESS", Mergeable: "MERGEABLE", ReviewDecision: "REVIEW_REQUIRED"},
			want: PRPhaseAwaitingApproval,
		},
		{
			name: "no signals → awaiting approval",
			sig:  prSignals{},
			want: PRPhaseAwaitingApproval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := computePRPhase(tt.sig); got != tt.want {
				t.Errorf("computePRPhase(%+v) = %q, want %q", tt.sig, got, tt.want)
			}
		})
	}
}
