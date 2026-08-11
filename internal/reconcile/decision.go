package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

func Decide(s Snapshot) Plan {
	plan := Plan{
		Preconditions: Preconditions{
			TaskGeneration:     s.TaskGeneration,
			WorkflowGeneration: s.WorkflowGeneration,
			LeaseID:            s.Lease.ID,
			RunID:              s.Run.ID,
			LocalSHA:           s.Git.HeadSHA,
			RemoteSHA:          s.Git.RemoteSHA,
			PRHeadSHA:          s.PR.HeadSHA,
			SidecarsDigest:     digestSidecars(s.Sidecars),
			EvidenceDigest:     digestEvidence(s.Evidence.Items),
		},
		Cleanup: PreservationProof{
			TaskGeneration: s.TaskGeneration,
			LeaseID:        s.Lease.ID,
			ObservedSHA:    s.Git.HeadSHA,
			NoDirtyWork:    s.Git.Available && s.Git.Healthy && !s.Git.Dirty && !s.Git.Staged && s.Git.Operation == "",
			NoLocalOnlyWork: s.Git.Available && s.Git.Healthy && s.Git.HeadExists &&
				((s.Git.RemoteSHA != "" && s.Git.Ahead == 0 && (s.Git.RemoteSHA == s.Git.HeadSHA || s.Git.Behind > 0)) ||
					(s.Git.RemoteSHA == "" && s.Git.BaseSHA != "" && s.Git.HeadSHA == s.Git.BaseSHA)),
		},
	}

	return decide(s, plan)
}

func digestSidecars(items []SidecarState) string {
	rows := make([]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, item.Name+"\x00"+item.Digest)
	}
	sort.Strings(rows)
	return digestRows(rows)
}

func digestEvidence(items []EvidenceItem) string {
	rows := make([]string, 0, len(items))
	for _, item := range items {
		passed := "false"
		if item.Passed {
			passed = "true"
		}
		rows = append(rows, item.Criterion+"\x00"+item.SourceSHA+"\x00"+passed)
	}
	sort.Strings(rows)
	return digestRows(rows)
}

func digestRows(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:])
}

func decide(s Snapshot, plan Plan) Plan {
	if s.Lease.Required && (!s.Lease.Current || s.Lease.ID == "") {
		return withAction(plan, ActionWait, "stale or missing attempt lease")
	}
	if s.Evidence.Required && (!s.Evidence.Verified || s.Evidence.SourceSHA != s.Git.HeadSHA) {
		return withAction(plan, ActionWait, "verification evidence does not match observed HEAD")
	}
	if !s.Git.Available {
		if s.PR.Number > 0 && strings.EqualFold(s.PR.State, "OPEN") && strings.EqualFold(s.PR.Mergeable, "MERGEABLE") {
			return withDelivery(plan, ActionResumeMergeablePR, "live PR is mergeable without a local checkout")
		}
		return withAction(plan, ActionWait, "authoritative worktree is unavailable")
	}
	if !s.Git.Healthy || !s.Git.HeadExists {
		return withAction(plan, ActionQuarantine, "Git administration or HEAD object is unhealthy")
	}
	if s.Git.Operation != "" {
		return withAction(plan, ActionRepair, "Git operation requires bounded recovery: "+s.Git.Operation)
	}
	if s.Git.Dirty || s.Git.Staged {
		return withAction(plan, ActionCheckpoint, "uncommitted author work must be checkpointed")
	}
	// A watchdog/restart may observe a non-terminal provider run after the live
	// PR already contains the authoritative work. Git/PR state, not missing
	// provider prose, owns this no-op recovery decision.
	if strings.EqualFold(s.PR.State, "OPEN") && strings.EqualFold(s.PR.Mergeable, "MERGEABLE") && noAdditiveLocalWork(s) {
		return withDelivery(plan, ActionResumeMergeablePR, "live PR is mergeable and recovery produced no additive work")
	}
	if !s.Run.Terminal && s.Intent != IntentCleanup {
		return withAction(plan, ActionWait, "provider run has no terminal outcome")
	}

	switch strings.ToUpper(s.PR.State) {
	case "MERGED":
		if s.Git.RemoteSHA != "" && s.Git.RemoteSHA != s.Git.HeadSHA {
			return withAction(plan, ActionAdoptRemote, "PR branch contains newer reachable work")
		}
		return withDelivery(plan, ActionAdvance, "PR is already merged and local task work is preserved")
	case "CLOSED":
		return withAction(plan, ActionHumanDecision, "PR was closed without merge")
	case "OPEN":
		if strings.EqualFold(s.PR.Mergeable, "CONFLICTING") {
			return withAction(plan, ActionRepair, "live PR has content conflicts")
		}
	}

	if s.Git.RemoteSHA == "" && s.Git.TaskWorkReachable {
		return withAction(plan, ActionPush, "reachable task work has no remote branch")
	}
	if s.Git.RemoteSHA != "" && s.Git.RemoteSHA != s.Git.HeadSHA {
		if s.Git.Ahead > 0 {
			return withAction(plan, ActionPush, "local task work is ahead of the remote branch")
		}
		if s.Git.Behind > 0 {
			return withAction(plan, ActionAdoptRemote, "remote branch contains newer reachable work")
		}
		return withAction(plan, ActionRepair, "local and remote branch histories diverged")
	}
	if s.Git.TreeEquivalentToBase && !s.Git.TaskWorkReachable {
		if s.Run.Success {
			// A clean successful author run with no repository delta is not a Git
			// conflict. Deliver it to the workflow, whose verify_commits step owns
			// the bounded retry/escalation policy for no-commit outcomes. Treating
			// it as repair launches branch-conflict recovery against a clean tree
			// and loops forever for intentionally external/no-code tasks.
			return withDelivery(plan, ActionAdvance, "successful run produced no repository changes; workflow owns no-commit policy")
		}
		return withDelivery(plan, ActionRepair, "terminal author run failed without preserving task work")
	}
	if !s.Run.Success {
		return withDelivery(plan, ActionRepair, "terminal author run failed")
	}
	return withDelivery(plan, ActionAdvance, "observed state is safe to advance")
}

func noAdditiveLocalWork(s Snapshot) bool {
	return s.Git.RemoteSHA != "" && s.Git.Ahead == 0
}

func withAction(plan Plan, action Action, reason string) Plan {
	plan.Action = action
	plan.Reason = reason
	return plan
}

func withDelivery(plan Plan, action Action, reason string) Plan {
	plan = withAction(plan, action, reason)
	plan.DeliverRunOutcome = true
	return plan
}
