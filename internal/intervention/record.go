package intervention

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

const (
	projectKeySalt  = "sybra-intervention-v1"
	recordIDKeySalt = "sybra-intervention-record-v1"
)

// ProjectKey returns the Store partition key for proj: the plain project ID
// for a public/pet project, or an opaque per-project hash for a work
// project — same shape as experience.ProjectKey, kept as an independent
// implementation (rather than a cross-package call) so intervention has no
// dependency on the experience package.
func ProjectKey(proj project.Project) string {
	if proj.Type != project.ProjectTypeWork {
		return strings.TrimSpace(proj.ID)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		projectKeySalt,
		proj.ID,
		proj.Owner,
		proj.Repo,
		proj.URL,
	}, "\x00")))
	return "work-" + hex.EncodeToString(sum[:])
}

// WorkRecordID returns an opaque, deterministic stand-in for a work-typed
// task's TaskID field so a persisted work record never carries the plain
// Sybra task ID as a correlation handle back to the originating work task.
func WorkRecordID(taskID string) string {
	sum := sha256.Sum256([]byte(recordIDKeySalt + "\x00" + strings.TrimSpace(taskID)))
	return "work-task-" + hex.EncodeToString(sum[:])
}

// FromUnblock derives a Record from a task as it stood immediately before a
// human-required unblock write (cur), the write's own target status/reason,
// and the caller-supplied OperatorActionClass — the caller (one of the real
// exit paths from human-required, see Capture) knows whether the transition
// was a human clicking Dispatch or an automated recovery path re-entering
// the workflow, so classification lives at the call site, not here.
func FromUnblock(cur task.Task, proj project.Project, target, reason string, class OperatorActionClass, now time.Time) Record {
	var workflowStep string
	if cur.Workflow != nil {
		workflowStep = cur.Workflow.CurrentStep
	}
	rec := Record{
		TaskID:              cur.ID,
		CreatedAt:           now,
		ProjectID:           proj.ID,
		ProjectType:         string(proj.Type),
		BlockerKind:         string(cur.Blocker.Kind),
		BlockerCode:         cur.Blocker.Code,
		FromStatus:          string(cur.Status),
		ToStatus:            target,
		WorkflowStep:        workflowStep,
		AttemptedActions:    attemptedActions(cur),
		OperatorActionClass: class,
		OperatorReason:      strings.TrimSpace(reason),
		ReplayStatus:        ReplayStatusUnsupportedSimulation,
	}
	rec.Fingerprint = Fingerprint(rec)
	return rec
}

// attemptedActions summarizes what the system already tried before this task
// was parked human-required: the role of every prior agent run plus the
// terminal status_reason, deduplicated and order-preserving. Deliberately
// reads only fields already resident on cur — no synchronous audit read.
func attemptedActions(cur task.Task) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for i := range cur.AgentRuns {
		add(cur.AgentRuns[i].Role)
	}
	add(cur.StatusReason)
	return out
}
