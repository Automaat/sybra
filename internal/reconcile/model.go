// Package reconcile turns observed task, run, Git, PR, and verification state
// into deterministic post-run action plans. It deliberately contains no I/O:
// callers must observe a fresh Snapshot and apply the returned Preconditions
// atomically before performing an effect.
package reconcile

import "context"

type Intent string

const (
	IntentAuthorCompletion Intent = "author-completion"
	IntentFixReview        Intent = "fix-review"
	IntentPRFix            Intent = "pr-fix"
	IntentHumanRecovery    Intent = "human-recovery"
	IntentStaleRun         Intent = "stale-run"
	IntentRestart          Intent = "restart"
	IntentCleanup          Intent = "cleanup"
)

type Action string

const (
	ActionAdvance           Action = "advance"
	ActionCheckpoint        Action = "checkpoint"
	ActionPush              Action = "push"
	ActionAdoptRemote       Action = "adopt-remote"
	ActionResumeMergeablePR Action = "resume-mergeable-pr"
	ActionRepair            Action = "repair"
	ActionQuarantine        Action = "quarantine"
	ActionWait              Action = "wait"
	ActionHumanDecision     Action = "human-decision"
)

type Snapshot struct {
	Intent Intent

	TaskID             string
	TaskGeneration     int64
	WorkflowGeneration int64
	WorkflowID         string
	WorkflowStep       string

	Lease    LeaseState
	Run      RunState
	Git      GitState
	PR       PRState
	Evidence EvidenceState
	Sidecars []SidecarState

	Replay bool
}

type LeaseState struct {
	ID       string
	Required bool
	Current  bool
}

type RunState struct {
	ID       string
	Role     string
	Terminal bool
	Success  bool
}

type GitState struct {
	Available bool
	Healthy   bool

	Branch    string
	HeadSHA   string
	BaseSHA   string
	RemoteSHA string
	PRHeadSHA string

	HeadExists           bool
	Dirty                bool
	Staged               bool
	Operation            string
	Ahead                int
	Behind               int
	TaskWorkReachable    bool
	TreeEquivalentToBase bool
	PushForbidden        bool
}

type PRState struct {
	Number          int
	State           string
	HeadSHA         string
	BaseSHA         string
	Mergeable       string
	Checks          string
	ReviewsResolved bool
}

type EvidenceState struct {
	Required  bool
	SourceSHA string
	Verified  bool
	Items     []EvidenceItem
}

// EvidenceItem is one durable verification result bound to the revision it
// actually inspected. SourceSHA is deliberately explicit: an accepted result
// for an older tree must never be confused with evidence for the current HEAD.
type EvidenceItem struct {
	Criterion string
	SourceSHA string
	Passed    bool
}

// SidecarState records the content identity of a task sidecar observed during
// reconciliation. The pure decision does not interpret sidecar prose; it only
// carries stable digests into effect preconditions so a concurrent import or
// rewrite forces re-observation.
type SidecarState struct {
	Name   string
	Digest string
}

type Preconditions struct {
	TaskGeneration     int64
	WorkflowGeneration int64
	LeaseID            string
	RunID              string
	LocalSHA           string
	RemoteSHA          string
	PRHeadSHA          string
	SidecarsDigest     string
	EvidenceDigest     string
}

type PreservationProof struct {
	TaskGeneration  int64
	LeaseID         string
	ObservedSHA     string
	NoDirtyWork     bool
	NoLocalOnlyWork bool
}

type Plan struct {
	Action            Action
	Reason            string
	DeliverRunOutcome bool
	Preconditions     Preconditions
	Cleanup           PreservationProof
}

type Request struct {
	TaskID string
	RunID  string
	Intent Intent
}

// Runner is the single post-run reconciliation seam used by live completion,
// reattach, and stale-run recovery. Implementations must return a plan derived
// from a fresh observed snapshot; nil means reconciliation is disabled only in
// deliberately partial test/degraded wiring.
type Runner interface {
	Reconcile(context.Context, Request) (Plan, error)
}

func (p Plan) AllowsCleanup() bool {
	return p.Cleanup.NoDirtyWork && p.Cleanup.NoLocalOnlyWork && p.Cleanup.ObservedSHA != ""
}
