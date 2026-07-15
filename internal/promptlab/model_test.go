package promptlab

import "testing"

func TestProposalIDStable(t *testing.T) {
	s := WeakSubject{
		Subject:    Subject{Role: "implementation"},
		Samples:    12,
		EffectSize: 0.234,
	}
	id1 := proposalID(s, "tighten-instructions")
	id2 := proposalID(s, "tighten-instructions")
	if id1 != id2 {
		t.Fatalf("proposalID not stable: %q != %q", id1, id2)
	}
	if id1 == proposalID(s, "restructure-context") {
		t.Fatalf("proposalID collided across distinct intents")
	}
	drifted := s
	drifted.Samples = 13
	drifted.EffectSize = 0.301
	if id1 != proposalID(drifted, "tighten-instructions") {
		t.Fatalf("proposalID must stay stable as evidence magnitude drifts")
	}

	other := s
	other.WorkflowStep = "review"
	if id1 == proposalID(other, "tighten-instructions") {
		t.Fatalf("proposalID collided across distinct workflow steps")
	}
}

func TestStubEvaluatorForcesHumanApproval(t *testing.T) {
	p := Proposal{Candidate: VariantCandidate{ID: "pl-abc", Intent: "tighten-instructions"}}
	got := EvaluateProposal(p, stubEvaluator{})
	if got.Offline.Verdict != VerdictNoVerdict {
		t.Fatalf("verdict = %q, want %q", got.Offline.Verdict, VerdictNoVerdict)
	}
	if !got.RequiresHumanApproval {
		t.Fatal("stub NoVerdict must require human approval (fail-closed)")
	}
}

func TestEvaluateProposalFailedRequiresHumanApproval(t *testing.T) {
	p := Proposal{Candidate: VariantCandidate{ID: "pl-abc"}}
	failing := fixedEvaluator{OfflineResult{Verdict: VerdictFailed, Reason: "regression"}}
	got := EvaluateProposal(p, failing)
	if !got.RequiresHumanApproval {
		t.Fatal("Failed verdict must require human approval")
	}
}

func TestEvaluateProposalPassedDoesNotRequireApproval(t *testing.T) {
	p := Proposal{Candidate: VariantCandidate{ID: "pl-abc"}}
	passing := fixedEvaluator{OfflineResult{Verdict: VerdictPassed}}
	got := EvaluateProposal(p, passing)
	if got.RequiresHumanApproval {
		t.Fatal("Passed verdict must not require human approval")
	}
}

type fixedEvaluator struct{ result OfflineResult }

func (f fixedEvaluator) Evaluate(VariantCandidate) OfflineResult { return f.result }
