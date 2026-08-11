package reconcile

import (
	"reflect"
	"testing"
)

func baselineSnapshot() Snapshot {
	return Snapshot{
		Intent: IntentAuthorCompletion, TaskID: "task-1", TaskGeneration: 7, WorkflowGeneration: 7,
		Lease: LeaseState{ID: "agent-1", Required: true, Current: true},
		Run:   RunState{ID: "agent-1", Role: "implementation", Terminal: true, Success: true},
		Git:   GitState{Available: true, Healthy: true, HeadSHA: "head", BaseSHA: "base", RemoteSHA: "head", HeadExists: true, TaskWorkReachable: true},
	}
}

func TestDecideMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Snapshot)
		want   Action
	}{
		{"clean equal advances", func(*Snapshot) {}, ActionAdvance},
		{"stale lease waits", func(s *Snapshot) { s.Lease.Current = false }, ActionWait},
		{"dirty checkpoints", func(s *Snapshot) { s.Git.Dirty = true }, ActionCheckpoint},
		{"staged checkpoints", func(s *Snapshot) { s.Git.Staged = true }, ActionCheckpoint},
		{"merge conflict repairs", func(s *Snapshot) { s.Git.Operation = "merge" }, ActionRepair},
		{"bad git quarantines", func(s *Snapshot) { s.Git.Healthy = false }, ActionQuarantine},
		{"missing remote pushes", func(s *Snapshot) { s.Git.RemoteSHA = "" }, ActionPush},
		{"local ahead pushes", func(s *Snapshot) { s.Git.RemoteSHA = "old"; s.Git.Ahead = 1 }, ActionPush},
		{"remote ahead adopts", func(s *Snapshot) { s.Git.RemoteSHA = "remote-new-sha"; s.Git.Behind = 1 }, ActionAdoptRemote},
		{"diverged repairs", func(s *Snapshot) { s.Git.RemoteSHA = "other" }, ActionRepair},
		{"successful empty equivalent advances to workflow policy", func(s *Snapshot) { s.Git.TreeEquivalentToBase = true; s.Git.TaskWorkReachable = false }, ActionAdvance},
		{"failed empty equivalent delivers bounded workflow repair", func(s *Snapshot) {
			s.Git.TreeEquivalentToBase = true
			s.Git.TaskWorkReachable = false
			s.Run.Success = false
		}, ActionRepair},
		{"reachable equivalent advances", func(s *Snapshot) { s.Git.TreeEquivalentToBase = true }, ActionAdvance},
		{"mergeable no-op resumes", func(s *Snapshot) {
			s.PR = PRState{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadSHA: "head"}
			s.Git.Ahead = 0
		}, ActionResumeMergeablePR},
		{"mergeable PR without observed remote does not hide local state", func(s *Snapshot) {
			s.PR = PRState{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadSHA: "remote-head"}
			s.Git.RemoteSHA = ""
			s.Git.Ahead = 0
		}, ActionPush},
		{"mergeable PR with base-equivalent local history still pushes", func(s *Snapshot) {
			s.PR = PRState{Number: 1, State: "OPEN", Mergeable: "MERGEABLE", HeadSHA: "remote-head"}
			s.Git.RemoteSHA = ""
			s.Git.TreeEquivalentToBase = true
		}, ActionPush},
		{"conflicted PR repairs", func(s *Snapshot) { s.PR = PRState{Number: 1, State: "OPEN", Mergeable: "CONFLICTING"} }, ActionRepair},
		{"closed PR asks human", func(s *Snapshot) { s.PR = PRState{Number: 1, State: "CLOSED"} }, ActionHumanDecision},
		{"merged PR advances when branch is already preserved", func(s *Snapshot) { s.PR = PRState{Number: 1, State: "MERGED"} }, ActionAdvance},
		{"evidence mismatch waits", func(s *Snapshot) { s.Evidence = EvidenceState{Required: true, SourceSHA: "old", Verified: true} }, ActionWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := baselineSnapshot()
			tc.mutate(&s)
			if got := Decide(s).Action; got != tc.want {
				t.Fatalf("Decide().Action = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecideOnlyDeliversSafeOrFailedRunOutcomes(t *testing.T) {
	t.Parallel()
	s := baselineSnapshot()
	if plan := Decide(s); !plan.DeliverRunOutcome {
		t.Fatalf("advance plan did not deliver outcome: %#v", plan)
	}
	s.Git.Operation = "merge"
	if plan := Decide(s); plan.DeliverRunOutcome || plan.Action != ActionRepair {
		t.Fatalf("unresolved Git operation was delivered: %#v", plan)
	}
	s.Git.Operation = ""
	s.Run.Success = false
	if plan := Decide(s); !plan.DeliverRunOutcome || plan.Action != ActionRepair {
		t.Fatalf("failed run was not delivered to bounded workflow retry: %#v", plan)
	}
}

func TestDecideCarriesEveryMutationPrecondition(t *testing.T) {
	t.Parallel()
	s := baselineSnapshot()
	s.PR.HeadSHA = "pr-head"
	s.Sidecars = []SidecarState{{Name: "review", Digest: "sidecar-digest"}}
	s.Evidence.Items = []EvidenceItem{{Criterion: "verify", SourceSHA: "head", Passed: true}}
	p := Decide(s)
	want := Preconditions{
		TaskGeneration: 7, WorkflowGeneration: 7, LeaseID: "agent-1", RunID: "agent-1",
		LocalSHA: "head", RemoteSHA: "head", PRHeadSHA: "pr-head",
		SidecarsDigest: digestSidecars(s.Sidecars), EvidenceDigest: digestEvidence(s.Evidence.Items),
	}
	if !reflect.DeepEqual(p.Preconditions, want) {
		t.Fatalf("preconditions = %#v, want %#v", p.Preconditions, want)
	}
}

func TestObservationDigestsAreOrderIndependentAndContentSensitive(t *testing.T) {
	t.Parallel()
	a := baselineSnapshot()
	a.Sidecars = []SidecarState{{Name: "plan", Digest: "a"}, {Name: "review", Digest: "b"}}
	a.Evidence.Items = []EvidenceItem{{Criterion: "test", SourceSHA: "head", Passed: true}, {Criterion: "review", SourceSHA: "head", Passed: true}}
	b := a
	b.Sidecars = []SidecarState{a.Sidecars[1], a.Sidecars[0]}
	b.Evidence.Items = []EvidenceItem{a.Evidence.Items[1], a.Evidence.Items[0]}
	if Decide(a).Preconditions != Decide(b).Preconditions {
		t.Fatal("equivalent observations produced order-dependent preconditions")
	}
	b.Evidence.Items[0].Passed = false
	if Decide(a).Preconditions.EvidenceDigest == Decide(b).Preconditions.EvidenceDigest {
		t.Fatal("changed verification evidence did not change its precondition digest")
	}
}

func TestCleanupProofFailsClosed(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.Git.Dirty = true },
		func(s *Snapshot) { s.Git.Staged = true },
		func(s *Snapshot) { s.Git.Operation = "rebase" },
		func(s *Snapshot) { s.Git.Ahead = 1 },
		func(s *Snapshot) { s.Git.RemoteSHA = "other" },
		func(s *Snapshot) { s.Git.RemoteSHA = ""; s.Git.TaskWorkReachable = true },
		func(s *Snapshot) { s.Git.Healthy = false },
		func(s *Snapshot) { s.Git.HeadExists = false },
	} {
		s := baselineSnapshot()
		mutate(&s)
		if Decide(s).AllowsCleanup() {
			t.Fatalf("unsafe snapshot unexpectedly allowed cleanup: %#v", s.Git)
		}
	}
}

func TestCleanupProofAllowsProvenRemoteStateWithNoTaskWork(t *testing.T) {
	t.Parallel()
	s := baselineSnapshot()
	s.Git.TaskWorkReachable = false
	if !Decide(s).AllowsCleanup() {
		t.Fatal("clean remote-equal snapshot without task work did not allow cleanup")
	}
}

func TestCleanupProofFailsClosedWithoutAnAuthoritativeBaseOrRemote(t *testing.T) {
	t.Parallel()
	s := baselineSnapshot()
	s.Git.RemoteSHA = ""
	s.Git.BaseSHA = ""
	s.Git.Ahead = 0
	s.Git.TaskWorkReachable = false
	if plan := Decide(s); plan.Cleanup.NoLocalOnlyWork || plan.AllowsCleanup() {
		t.Fatalf("unknown base and remote approved cleanup: %#v", plan)
	}

	s.Git.BaseSHA = s.Git.HeadSHA
	if plan := Decide(s); !plan.Cleanup.NoLocalOnlyWork || !plan.AllowsCleanup() {
		t.Fatalf("known unchanged base rejected cleanup: %#v", plan)
	}
}

func FuzzDecideDeterministic(f *testing.F) {
	f.Add(true, true, false, false, 0, 0, "OPEN", "MERGEABLE")
	f.Fuzz(func(t *testing.T, lease, healthy, dirty, staged bool, ahead, behind int, prState, mergeable string) {
		s := baselineSnapshot()
		s.Lease.Current = lease
		s.Git.Healthy = healthy
		s.Git.Dirty = dirty
		s.Git.Staged = staged
		s.Git.Ahead = ahead
		s.Git.Behind = behind
		s.PR = PRState{Number: 1, State: prState, Mergeable: mergeable}
		if a, b := Decide(s), Decide(s); !reflect.DeepEqual(a, b) {
			t.Fatalf("Decide is nondeterministic: %#v != %#v", a, b)
		}
	})
}
