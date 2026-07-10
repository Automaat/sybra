// Package promptlab identifies underperforming prompt/skill subjects from
// fleet evidence (the Evaluation report + stats run records) and scaffolds
// reviewable proposals for new versioned variants. It never authors variant
// text and never mutates a production prompt or skill file — proposals are
// filed as local Sybra tasks for a human (or a later agent run) to flesh out
// and gate through the normal offline-eval + A/B path.
package promptlab

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// ProposalTag marks local Sybra tasks filed from Prompt Lab proposals. These
// tasks are reviewed and advanced by PromptLabService, not by normal triage or
// task.created workflow dispatch.
const ProposalTag = "prompt-lab-proposal"

// Subject identifies a role/workflow-step slice of the fleet whose
// prompt/skill may be underperforming.
type Subject struct {
	Role         string `json:"role"`
	WorkflowStep string `json:"workflowStep,omitempty"`
}

// WeakSubject is a Subject flagged by fleet evidence, gated on both sample
// size and effect size so a single noisy run never triggers a proposal.
type WeakSubject struct {
	Subject
	Metric     string   `json:"metric"`
	Detail     string   `json:"detail"`
	Samples    int      `json:"samples"`
	EffectSize float64  `json:"effectSize"`
	ProjectIDs []string `json:"projectIds,omitempty"`
	TaskIDs    []string `json:"taskIds,omitempty"`
}

// VariantCandidate is a versioned, content-addressed scaffold for a future
// prompt/skill variant. Intent records only the DIRECTION of the change —
// promptlab never authors the variant's actual text; that happens later, by
// hand or by an agent, in the task this candidate is filed under.
type VariantCandidate struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
}

// OfflineVerdict is the tri-state result of screening a VariantCandidate.
type OfflineVerdict string

const (
	VerdictPassed    OfflineVerdict = "passed"
	VerdictFailed    OfflineVerdict = "failed"
	VerdictNoVerdict OfflineVerdict = "no-verdict"
)

// OfflineResult is the outcome of running a VariantCandidate through an
// OfflineEvaluator.
type OfflineResult struct {
	Verdict OfflineVerdict `json:"verdict"`
	Reason  string         `json:"reason,omitempty"`
}

// OfflineEvaluator screens a VariantCandidate before its proposal is filed.
//
// promptlab proposals are scaffolds (rationale + expected impact — see
// propose.go), not authored prompt/skill bytes, so there is nothing yet to
// run through internal/prompteval's real runner: that screening happens once
// a human or agent authors the variant text inside the filed task.
// stubEvaluator reflects that by always returning NoVerdict, which
// evaluate.go treats the same as Failed — fail-closed until a real candidate
// with resolved text exists.
type OfflineEvaluator interface {
	Evaluate(VariantCandidate) OfflineResult
}

type stubEvaluator struct{}

func (stubEvaluator) Evaluate(VariantCandidate) OfflineResult {
	return OfflineResult{
		Verdict: VerdictNoVerdict,
		Reason:  "no resolved prompt/skill text to evaluate yet; author it in the filed task",
	}
}

// Proposal is one reviewable suggestion to create a new prompt/skill variant,
// scaffolded from fleet evidence. It never mutates a production prompt or
// skill file itself.
type Proposal struct {
	ID                    string           `json:"id"`
	Subject               Subject          `json:"subject"`
	Title                 string           `json:"title"`
	Rationale             string           `json:"rationale"`
	ExpectedImpact        string           `json:"expectedImpact"`
	Candidate             VariantCandidate `json:"candidate"`
	Evidence              WeakSubject      `json:"evidence"`
	Offline               OfflineResult    `json:"offline"`
	RequiresHumanApproval bool             `json:"requiresHumanApproval"`
	CreatedAt             time.Time        `json:"createdAt"`
}

// RunResult is the persisted, filed, and CLI-printed output of one promptlab run.
type RunResult struct {
	GeneratedAt  time.Time     `json:"generatedAt"`
	WeakSubjects []WeakSubject `json:"weakSubjects"`
	Proposals    []Proposal    `json:"proposals"`
	Dropped      int           `json:"dropped,omitempty"`
}

func proposalID(s WeakSubject, intent string) string {
	raw := fmt.Sprintf("%s|%s|%s", s.Role, s.WorkflowStep, intent)
	sum := sha256.Sum256([]byte(raw))
	return "pl-" + hex.EncodeToString(sum[:])[:12]
}
