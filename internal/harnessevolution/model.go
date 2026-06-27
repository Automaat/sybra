package harnessevolution

import "time"

type ProposalKind string

const (
	KindPromptChange       ProposalKind = "prompt_change"
	KindWorkflowTopology   ProposalKind = "workflow_topology_change"
	KindValidatorChange    ProposalKind = "validator_change"
	KindRetryLimitChange   ProposalKind = "retry_limit_change"
	KindPermissionPolicy   ProposalKind = "permission_policy_change"
	KindContextPacking     ProposalKind = "context_packing_change"
	KindNetworkAccess      ProposalKind = "network_access_change"
	KindSecretHandling     ProposalKind = "secret_handling_change"
	KindDeploymentBehavior ProposalKind = "deployment_behavior_change"
	KindHumanReviewGate    ProposalKind = "human_review_gate_change"
)

type RiskClass string

const (
	RiskStandard RiskClass = "standard_review"
	RiskHuman    RiskClass = "requires_human_approval"
)

type Recommendation string

const (
	RecommendationRecommend        Recommendation = "recommend"
	RecommendationNeedsHumanReview Recommendation = "needs_human_review"
	RecommendationReject           Recommendation = "reject"
)

type FailureEvent struct {
	TraceID      string    `json:"traceId"`
	TaskID       string    `json:"taskId,omitempty"`
	AgentID      string    `json:"agentId,omitempty"`
	Role         string    `json:"role,omitempty"`
	Provider     string    `json:"provider,omitempty"`
	Category     string    `json:"category"`
	WorkflowStep string    `json:"workflowStep"`
	FailureKind  string    `json:"failureKind"`
	Fingerprint  string    `json:"fingerprint,omitempty"`
	OccurredAt   time.Time `json:"occurredAt"`
}

type Cluster struct {
	Key          string         `json:"key"`
	Cause        string         `json:"cause"`
	Count        int            `json:"count"`
	Events       []FailureEvent `json:"events"`
	FirstSeen    time.Time      `json:"firstSeen"`
	LastSeen     time.Time      `json:"lastSeen"`
	AffectedStep string         `json:"affectedStep"`
	Category     string         `json:"category"`
	FailureKind  string         `json:"failureKind"`
}

type EvidenceRef struct {
	TraceID      string    `json:"traceId"`
	TaskID       string    `json:"taskId,omitempty"`
	AgentID      string    `json:"agentId,omitempty"`
	WorkflowStep string    `json:"workflowStep"`
	Fingerprint  string    `json:"fingerprint,omitempty"`
	OccurredAt   time.Time `json:"occurredAt"`
}

type EvaluationResult struct {
	Recommendation Recommendation `json:"recommendation"`
	CasesRun       int            `json:"casesRun"`
	MatchedCases   []string       `json:"matchedCases,omitempty"`
	Failures       []string       `json:"failures,omitempty"`
}

type Proposal struct {
	ID                    string           `json:"id"`
	ClusterKey            string           `json:"clusterKey"`
	Kind                  ProposalKind     `json:"kind"`
	Title                 string           `json:"title"`
	ExpectedImpact        string           `json:"expectedImpact"`
	Risk                  RiskClass        `json:"risk"`
	RequiresHumanApproval bool             `json:"requiresHumanApproval"`
	Evidence              []EvidenceRef    `json:"evidence"`
	Evaluation            EvaluationResult `json:"evaluation"`
	CreatedAt             time.Time        `json:"createdAt"`
}

type RunResult struct {
	GeneratedAt time.Time  `json:"generatedAt"`
	Events      int        `json:"events"`
	Clusters    []Cluster  `json:"clusters"`
	Proposals   []Proposal `json:"proposals"`
}

func RequiresHumanApproval(kind ProposalKind) bool {
	switch kind {
	case KindPermissionPolicy, KindNetworkAccess, KindSecretHandling,
		KindDeploymentBehavior, KindHumanReviewGate:
		return true
	default:
		return false
	}
}
