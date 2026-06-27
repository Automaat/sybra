package harnessevolution

import (
	"strings"
	"testing"
	"time"
)

func TestPropose_MarksSensitiveKindsHumanApproval(t *testing.T) {
	cluster := Cluster{
		Key:          "k1",
		Category:     "failure_rate",
		FailureKind:  "permission_denied",
		AffectedStep: "implement",
		Count:        2,
		Events: []FailureEvent{
			{TraceID: "trace-a", WorkflowStep: "implement", Category: "failure_rate", FailureKind: "permission_denied"},
			{TraceID: "trace-b", WorkflowStep: "implement", Category: "failure_rate", FailureKind: "permission_denied"},
		},
	}

	proposals := Propose([]Cluster{cluster}, time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC))
	if len(proposals) != 1 {
		t.Fatalf("proposals len = %d, want 1", len(proposals))
	}
	p := proposals[0]
	if p.Kind != KindPermissionPolicy {
		t.Fatalf("kind = %q, want %q", p.Kind, KindPermissionPolicy)
	}
	if !p.RequiresHumanApproval || p.Risk != RiskHuman {
		t.Fatalf("human approval/risk = %v/%q, want true/%q", p.RequiresHumanApproval, p.Risk, RiskHuman)
	}
}

func TestEvaluateProposal_BlocksHumanGateRecommendation(t *testing.T) {
	cluster := Cluster{
		Category:    "failure_rate",
		FailureKind: "permission_denied",
		Count:       2,
	}
	p := Proposal{
		Kind:                  KindPermissionPolicy,
		RequiresHumanApproval: true,
	}
	corpus := []CorpusCase{{
		ID:                    "permission",
		Category:              "failure_rate",
		FailureKind:           "permission_denied",
		WantKind:              KindPermissionPolicy,
		RequiresHumanApproval: true,
	}}

	got := EvaluateProposal(p, cluster, corpus)
	if got.Recommendation != RecommendationNeedsHumanReview {
		t.Fatalf("recommendation = %q, want %q", got.Recommendation, RecommendationNeedsHumanReview)
	}
	if got.CasesRun != 1 {
		t.Fatalf("cases run = %d, want 1", got.CasesRun)
	}
}

func TestRenderProposalBody_IncludesRequiredEvidence(t *testing.T) {
	cluster := Cluster{
		Key:          "k1",
		Cause:        "agent_retry_loop / step_retry_exhausted / step:implement",
		Count:        2,
		FirstSeen:    time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC),
		LastSeen:     time.Date(2026, 6, 27, 13, 0, 0, 0, time.UTC),
		AffectedStep: "implement",
		FailureKind:  "step_retry_exhausted",
	}
	p := Proposal{
		ID:             "he-abc",
		ClusterKey:     "k1",
		Kind:           KindRetryLimitChange,
		ExpectedImpact: "reduce loops",
		Evaluation:     EvaluationResult{Recommendation: RecommendationRecommend, CasesRun: 1},
		Evidence: []EvidenceRef{{
			TraceID:      "trace-a",
			TaskID:       "task-1",
			WorkflowStep: "implement",
		}},
	}

	body := RenderProposalBody(p, cluster)
	for _, want := range []string{"trace-a", "implement", "Expected impact", "Regression check", "Standard Sybra review"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
