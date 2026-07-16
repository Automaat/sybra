package promptlab

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// candidateIntents are the directions promptlab may scaffold from a weak
// subject: tightening guardrails/instructions, or restructuring what
// evidence/context the role is given. Real prompt/skill authoring happens
// later, in the filed task — see VariantCandidate.
var candidateIntents = []string{
	"tighten-instructions",
	"restructure-context",
}

// strongEffectSize is the EffectSize above which a subject gets both
// candidate intents scaffolded instead of one — proportionate review queue
// depth to how much evidence backs the subject.
const strongEffectSize = 0.30

// Propose scaffolds 1-2 VariantCandidate proposals per weak subject, then
// caps the total at maxProposals (<=0 means unbounded) by keeping the
// highest-effect-size proposals. Returns the number dropped by the cap so
// the caller can log it — proposals are never silently truncated.
func Propose(subjects []WeakSubject, maxProposals int, now time.Time) (proposals []Proposal, dropped int) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for i := range subjects {
		s := &subjects[i]
		intents := candidateIntents
		if s.EffectSize < strongEffectSize {
			intents = candidateIntents[:1]
		}
		for _, intent := range intents {
			proposals = append(proposals, buildProposal(*s, intent, now))
		}
	}
	sort.SliceStable(proposals, func(i, j int) bool { return proposals[i].Evidence.EffectSize > proposals[j].Evidence.EffectSize })

	if maxProposals > 0 && len(proposals) > maxProposals {
		dropped = len(proposals) - maxProposals
		proposals = proposals[:maxProposals]
	}
	return proposals, dropped
}

func buildProposal(s WeakSubject, intent string, now time.Time) Proposal {
	candidate := VariantCandidate{ID: proposalID(s, intent), Intent: intent}
	return Proposal{
		ID:             candidate.ID,
		Subject:        s.Subject,
		Title:          proposalTitle(s, intent),
		Rationale:      s.Detail,
		ExpectedImpact: expectedImpact(s, intent),
		Candidate:      candidate,
		Evidence:       s,
		CreatedAt:      now,
	}
}

func proposalTitle(s WeakSubject, intent string) string {
	base := fmt.Sprintf("Prompt Lab: %s for role %s", strings.ReplaceAll(intent, "-", " "), s.Role)
	if len(base) <= 80 {
		return base
	}
	return base[:77] + "..."
}

func expectedImpact(s WeakSubject, intent string) string {
	return fmt.Sprintf(
		"Reduce the %.0f-point failure-rate gap observed for role %s (over %d recent runs) by scaffolding a %s variant for offline eval and review.",
		s.EffectSize*100, s.Role, s.Samples, strings.ReplaceAll(intent, "-", " "),
	)
}

// RenderProposalBody renders the reviewable task body for a Proposal. Never
// includes authored prompt/skill text — only evidence, rationale, and the
// offline eval verdict that gates it.
func RenderProposalBody(p Proposal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Prompt Lab Proposal\n\n")
	fmt.Fprintf(&b, "**Proposal ID:** `%s`\n", p.ID)
	fmt.Fprintf(&b, "**Subject role:** `%s`\n", p.Subject.Role)
	fmt.Fprintf(&b, "**Candidate intent:** `%s`\n", p.Candidate.Intent)
	fmt.Fprintf(&b, "**Review gate:** %s\n\n", reviewGate(p))

	fmt.Fprintf(&b, "## Rationale\n\n%s\n\n", p.Rationale)

	fmt.Fprintf(&b, "## Evidence\n\n")
	fmt.Fprintf(&b, "- Metric: `%s`\n", p.Evidence.Metric)
	fmt.Fprintf(&b, "- Samples: %d\n", p.Evidence.Samples)
	fmt.Fprintf(&b, "- Effect size: %.3f\n", p.Evidence.EffectSize)
	for _, id := range p.Evidence.TaskIDs {
		fmt.Fprintf(&b, "- Task `%s`\n", id)
	}

	fmt.Fprintf(&b, "\n## Proposed change\n\n")
	fmt.Fprintf(&b, "Scaffold a new versioned prompt/skill variant for role `%s` (intent: `%s`). ", p.Subject.Role, p.Candidate.Intent)
	fmt.Fprintf(&b, "This proposal does not itself contain variant prompt/skill text and does not mutate any production prompt or skill file — author the variant text in this task, then run it through the offline eval gate before online A/B enrollment.\n\n")

	fmt.Fprintf(&b, "## Expected impact\n\n%s\n\n", p.ExpectedImpact)

	fmt.Fprintf(&b, "## Offline eval\n\n")
	fmt.Fprintf(&b, "- Verdict: `%s`\n", p.Offline.Verdict)
	if p.Offline.Reason != "" {
		fmt.Fprintf(&b, "- Reason: %s\n", p.Offline.Reason)
	}

	fmt.Fprintf(&b, "\n## Approval\n\n")
	if p.RequiresHumanApproval {
		fmt.Fprintf(&b, "REQUIRES HUMAN APPROVAL before this candidate is authored and enrolled.\n")
	} else {
		fmt.Fprintf(&b, "Standard Sybra review required before enrollment.\n")
	}
	return b.String()
}

func reviewGate(p Proposal) string {
	if p.RequiresHumanApproval {
		return "requires human approval"
	}
	return "standard review"
}

// ProposalIDMarker is the substring RenderProposalBody emits for a proposal's
// ID, and the key HasProposal matches on. It is defined next to the renderer
// so the marker format and its parser can never drift apart.
func ProposalIDMarker(proposalID string) string {
	return "Proposal ID:** `" + proposalID + "`"
}

// HasProposal reports whether proposalID should be suppressed rather than
// filed again, given the tasks already on the board.
//
// A live (non-terminal) proposal always suppresses: it is still being worked.
// A terminal one suppresses only until cooldown elapses from its last update.
// Both halves matter and pull against each other:
//
// Ignoring terminal tasks entirely is what let pl-a2d853b2c1d9 get filed four
// separate times — the ID is a stable hash of (role, step, intent), so every
// tick re-proposed it as earlier copies aged into done/cancelled.
//
// Suppressing on terminal tasks forever is the opposite failure: with only two
// candidate intents per subject, a role would get at most two proposals for the
// lifetime of the board and then go permanently silent — including when the
// shipped variant lost its A/B and the role is still weak. Auto-approve makes
// that worse, not better, by churning proposals to terminal in hours.
//
// The cooldown is the bounded-rate middle: re-proposing a subject is allowed,
// just not every tick. A zero or negative cooldown suppresses on any terminal
// task (never re-file).
func HasProposal(tasks []task.Task, proposalID string, cooldown time.Duration, now time.Time) bool {
	marker := ProposalIDMarker(proposalID)
	for i := range tasks {
		t := &tasks[i]
		if !slices.Contains(t.Tags, ProposalTag) || !strings.Contains(t.Body, marker) {
			continue
		}
		if !task.IsTerminalStatus(t.Status) {
			return true
		}
		if cooldown <= 0 || now.Sub(t.UpdatedAt) < cooldown {
			return true
		}
	}
	return false
}
