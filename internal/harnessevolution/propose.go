package harnessevolution

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

func Propose(clusters []Cluster, now time.Time) []Proposal {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	proposals := make([]Proposal, 0, len(clusters))
	for i := range clusters {
		c := clusters[i]
		kind := proposalKind(c)
		p := Proposal{
			ID:                    proposalID(c, kind),
			ClusterKey:            c.Key,
			Kind:                  kind,
			Title:                 proposalTitle(kind, c),
			ExpectedImpact:        expectedImpact(c),
			Risk:                  riskFor(kind),
			RequiresHumanApproval: RequiresHumanApproval(kind),
			Evidence:              evidenceRefs(c.Events),
			CreatedAt:             now.UTC(),
		}
		proposals = append(proposals, p)
	}
	return proposals
}

func proposalKind(c Cluster) ProposalKind {
	category := normalizeToken(c.Category)
	failure := normalizeToken(c.FailureKind)
	switch {
	case strings.Contains(failure, "permission") || strings.Contains(failure, "tool_denied"):
		return KindPermissionPolicy
	case strings.Contains(failure, "secret") || strings.Contains(failure, "creden"+"tial"):
		return KindSecretHandling
	case strings.Contains(failure, "network"):
		return KindNetworkAccess
	case strings.Contains(failure, "context"):
		return KindContextPacking
	case strings.Contains(category, "triage_mismatch"), strings.Contains(category, "status_bounce"),
		strings.Contains(category, "status_bottleneck"), strings.Contains(failure, "topology"):
		return KindWorkflowTopology
	case strings.Contains(failure, "validator"):
		return KindValidatorChange
	case strings.Contains(category, "agent_retry_loop"), strings.Contains(category, "workflow_loop"),
		strings.Contains(failure, "retry"), strings.Contains(failure, "rate_limit"), strings.Contains(failure, "overloaded"):
		return KindRetryLimitChange
	default:
		return KindPromptChange
	}
}

func riskFor(kind ProposalKind) RiskClass {
	if RequiresHumanApproval(kind) {
		return RiskHuman
	}
	return RiskStandard
}

func proposalID(c Cluster, kind ProposalKind) string {
	raw := string(kind) + "|" + c.Key + "|" + c.Cause
	sum := sha256.Sum256([]byte(raw))
	return "he-" + hex.EncodeToString(sum[:])[:12]
}

func proposalTitle(kind ProposalKind, c Cluster) string {
	base := fmt.Sprintf("Harness proposal: %s", strings.ReplaceAll(string(kind), "_", " "))
	if c.AffectedStep != "" {
		base += " at " + c.AffectedStep
	}
	if len(base) <= 80 {
		return base
	}
	return base[:77] + "..."
}

func expectedImpact(c Cluster) string {
	step := c.AffectedStep
	if step == "" {
		step = "the affected workflow step"
	}
	return fmt.Sprintf("Reduce recurring %s failures at %s across %d recent events by changing harness behavior under normal review.",
		c.FailureKind, step, c.Count)
}

func evidenceRefs(events []FailureEvent) []EvidenceRef {
	out := make([]EvidenceRef, 0, len(events))
	for i := range events {
		ev := events[i]
		out = append(out, EvidenceRef{
			TraceID:      ev.TraceID,
			TaskID:       ev.TaskID,
			AgentID:      ev.AgentID,
			WorkflowStep: ev.WorkflowStep,
			Fingerprint:  ev.Fingerprint,
			OccurredAt:   ev.OccurredAt,
		})
	}
	slices.SortFunc(out, func(a, b EvidenceRef) int {
		return strings.Compare(a.TraceID, b.TraceID)
	})
	return out
}

func RenderProposalBody(p Proposal, c Cluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Harness Proposal\n\n")
	fmt.Fprintf(&b, "**Proposal ID:** `%s`\n", p.ID)
	fmt.Fprintf(&b, "**Kind:** `%s`\n", p.Kind)
	fmt.Fprintf(&b, "**Cluster:** `%s`\n", p.ClusterKey)
	fmt.Fprintf(&b, "**Review gate:** %s\n\n", reviewGate(p))

	fmt.Fprintf(&b, "## What is wrong\n\n")
	fmt.Fprintf(&b, "Sybra observed %d recurring harness failures for `%s`.\n\n", c.Count, c.Cause)

	fmt.Fprintf(&b, "## Evidence\n\n")
	fmt.Fprintf(&b, "- Failing workflow step: `%s`\n", emptyAs(c.AffectedStep, "unknown"))
	fmt.Fprintf(&b, "- Failure kind: `%s`\n", emptyAs(c.FailureKind, "unknown"))
	fmt.Fprintf(&b, "- First seen: %s\n", c.FirstSeen.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- Last seen: %s\n", c.LastSeen.UTC().Format(time.RFC3339))
	for _, ev := range p.Evidence {
		fmt.Fprintf(&b, "- Trace `%s`", ev.TraceID)
		if ev.TaskID != "" {
			fmt.Fprintf(&b, " task `%s`", ev.TaskID)
		}
		if ev.WorkflowStep != "" {
			fmt.Fprintf(&b, " step `%s`", ev.WorkflowStep)
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "\n## Proposed change\n\n")
	fmt.Fprintf(&b, "Create a reviewed `%s` change targeted at the repeated failure cause. The proposal intentionally does not mutate prompts, workflows, permissions, retry policy, validators, or deployment behavior by itself.\n\n", p.Kind)

	fmt.Fprintf(&b, "## Expected impact\n\n%s\n\n", p.ExpectedImpact)

	fmt.Fprintf(&b, "## Regression check\n\n")
	fmt.Fprintf(&b, "- Recommendation: `%s`\n", p.Evaluation.Recommendation)
	fmt.Fprintf(&b, "- Cases run: %d\n", p.Evaluation.CasesRun)
	for _, failure := range p.Evaluation.Failures {
		fmt.Fprintf(&b, "- Failure: %s\n", failure)
	}

	fmt.Fprintf(&b, "\n## Approval\n\n")
	if p.RequiresHumanApproval {
		fmt.Fprintf(&b, "REQUIRES HUMAN APPROVAL before adoption. This proposal touches a high-leverage boundary.\n")
	} else {
		fmt.Fprintf(&b, "Standard Sybra review and CI required before adoption.\n")
	}
	return b.String()
}

func reviewGate(p Proposal) string {
	if p.RequiresHumanApproval {
		return "requires human approval"
	}
	return "standard review"
}

func emptyAs(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
