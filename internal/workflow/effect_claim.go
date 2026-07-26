package workflow

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	ErrEffectClaimConflict   = errors.New("workflow effect is claimed by another owner")
	ErrEffectAlreadyComplete = errors.New("workflow effect is already complete")
	ErrEffectClaimLost       = errors.New("workflow effect claim is not held by this owner")
)

const (
	effectPosStepAction = iota
	effectPosAsyncAdvance
)

type EffectClaim struct {
	EffectID EffectID
	Owner    string
	LeaseTTL time.Duration
	Now      time.Time
}

type EffectClaimResult struct {
	Workflow  *Execution
	Record    EffectRecord
	Acquired  bool
	Refreshed bool
	Completed bool
}

func (e *Engine) effectClaimForStep(t TaskInfo, stepID string, pos int) EffectClaim {
	if t.Workflow != nil {
		if existing, ok := t.Workflow.EffectIDForStep(stepID, pos); ok {
			currentSeq := executionStepSeq(t.Workflow)
			if rec, found := t.Workflow.effectRecord(existing); found && rec.ID.StepSeq == currentSeq {
				return EffectClaim{
					EffectID: existing,
					Owner:    e.ownerID,
					LeaseTTL: e.effectLeaseTTL,
					Now:      e.now(),
				}
			}
		}
	}
	return EffectClaim{
		EffectID: EffectID{
			Generation: t.Generation,
			StepSeq:    executionStepSeq(t.Workflow),
			StepID:     stepID,
			Pos:        pos,
		},
		Owner:    e.ownerID,
		LeaseTTL: e.effectLeaseTTL,
		Now:      e.now(),
	}
}

func (e *Engine) claimStepEffect(taskID string, t TaskInfo, stepID string, pos int) (*Execution, EffectID, error) {
	claim := e.effectClaimForStep(t, stepID, pos)
	result, err := e.tasks.ClaimWorkflowEffect(taskID, claim)
	if err != nil {
		return result.Workflow, claim.EffectID, err
	}
	return result.Workflow, claim.EffectID, nil
}

func (e *Engine) completeClaimedEffect(taskID string, effectID EffectID) (*Execution, error) {
	result, err := e.tasks.CompleteWorkflowEffect(taskID, EffectClaim{
		EffectID: effectID,
		Owner:    e.ownerID,
		LeaseTTL: e.effectLeaseTTL,
		Now:      e.now(),
	})
	if err != nil {
		return result.Workflow, err
	}
	return result.Workflow, nil
}

func effectClaimFence(err error) bool {
	return errors.Is(err, ErrEffectClaimConflict) ||
		errors.Is(err, ErrEffectAlreadyComplete) ||
		errors.Is(err, ErrEffectClaimLost)
}

func mergeClaimedEffectLog(active, claimed *Execution) *Execution {
	switch {
	case active == nil:
		if claimed == nil {
			return nil
		}
		return claimed.Clone()
	case claimed == nil:
		return active
	}
	active.EffectLog = slices.Clone(claimed.EffectLog)
	for i := range active.EffectLog {
		active.EffectLog[i] = cloneEffectRecord(active.EffectLog[i])
	}
	return active
}

func isAsyncWorkflowStep(stepType StepType) bool {
	switch stepType {
	case StepRunAgent, StepParallel, StepBestOfN, StepWaitHuman:
		return true
	default:
		return false
	}
}

func (c EffectClaim) validate() error {
	switch {
	case c.Owner == "":
		return errors.New("workflow effect claim owner is required")
	case c.EffectID.StepID == "":
		return fmt.Errorf("workflow effect claim id %q has empty step id", c.EffectID.String())
	case c.LeaseTTL <= 0:
		return fmt.Errorf("workflow effect claim lease ttl must be > 0, got %s", c.LeaseTTL)
	case c.Now.IsZero():
		return errors.New("workflow effect claim time is required")
	default:
		return nil
	}
}

func (e *Execution) ClaimEffect(claim EffectClaim) (EffectClaimResult, error) {
	if e == nil {
		return EffectClaimResult{}, errors.New("workflow execution is nil")
	}
	if err := claim.validate(); err != nil {
		return EffectClaimResult{}, err
	}

	rec, found := e.effectRecord(claim.EffectID)
	if !found {
		expiresAt := claim.Now.UTC().Add(claim.LeaseTTL)
		e.EffectLog = append(e.EffectLog, EffectRecord{
			ID:             claim.EffectID,
			IntentAt:       claim.Now.UTC(),
			Owner:          claim.Owner,
			LeaseExpiresAt: &expiresAt,
		})
		if len(e.EffectLog) > maxEffectLog {
			e.EffectLog = e.EffectLog[len(e.EffectLog)-maxEffectLog:]
		}
		rec = &e.EffectLog[len(e.EffectLog)-1]
		return EffectClaimResult{
			Record:    cloneEffectRecord(*rec),
			Acquired:  true,
			Refreshed: false,
		}, nil
	}
	if rec.CompletedAt != nil {
		return EffectClaimResult{
			Record:    cloneEffectRecord(*rec),
			Completed: true,
		}, ErrEffectAlreadyComplete
	}
	if rec.Owner != "" && rec.Owner != claim.Owner && rec.leaseActiveAt(claim.Now) {
		return EffectClaimResult{Record: cloneEffectRecord(*rec)}, ErrEffectClaimConflict
	}
	expiresAt := claim.Now.UTC().Add(claim.LeaseTTL)
	refreshed := rec.Owner == claim.Owner
	rec.Owner = claim.Owner
	rec.LeaseExpiresAt = &expiresAt
	if rec.IntentAt.IsZero() {
		rec.IntentAt = claim.Now.UTC()
	}
	return EffectClaimResult{
		Record:    cloneEffectRecord(*rec),
		Acquired:  true,
		Refreshed: refreshed,
	}, nil
}

func (e *Execution) CompleteEffect(claim EffectClaim) (EffectClaimResult, error) {
	if e == nil {
		return EffectClaimResult{}, errors.New("workflow execution is nil")
	}
	if err := claim.validate(); err != nil {
		return EffectClaimResult{}, err
	}

	rec, found := e.effectRecord(claim.EffectID)
	if !found {
		return EffectClaimResult{}, ErrEffectClaimLost
	}
	if rec.CompletedAt != nil {
		return EffectClaimResult{
			Record:    cloneEffectRecord(*rec),
			Completed: true,
		}, ErrEffectAlreadyComplete
	}
	if rec.Owner != claim.Owner || !rec.leaseActiveAt(claim.Now) {
		return EffectClaimResult{Record: cloneEffectRecord(*rec)}, ErrEffectClaimLost
	}
	completedAt := claim.Now.UTC()
	rec.CompletedAt = &completedAt
	return EffectClaimResult{
		Record:    cloneEffectRecord(*rec),
		Completed: true,
	}, nil
}

func (e *Execution) effectRecord(id EffectID) (*EffectRecord, bool) {
	for i := range e.EffectLog {
		if e.EffectLog[i].ID.Equal(id) {
			return &e.EffectLog[i], true
		}
	}
	return nil, false
}

func (r EffectRecord) leaseActiveAt(now time.Time) bool {
	if r.LeaseExpiresAt == nil {
		return false
	}
	return r.LeaseExpiresAt.After(now.UTC())
}

func cloneEffectRecord(rec EffectRecord) EffectRecord {
	out := rec
	if rec.LeaseExpiresAt != nil {
		t := *rec.LeaseExpiresAt
		out.LeaseExpiresAt = &t
	}
	if rec.CompletedAt != nil {
		t := *rec.CompletedAt
		out.CompletedAt = &t
	}
	return out
}
