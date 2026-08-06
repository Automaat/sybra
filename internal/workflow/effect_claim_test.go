package workflow

import (
	"errors"
	"testing"
	"time"
)

func TestEffectClaim_FirstOwnerWins(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

	result, err := exec.ClaimEffect(EffectClaim{
		EffectID: makeID("create_pr", effectPosStepAction),
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("ClaimEffect: %v", err)
	}
	if !result.Acquired || result.Refreshed {
		t.Fatalf("claim result = %+v, want acquired new lease", result)
	}
	if result.Record.Owner != "engine-a" {
		t.Fatalf("owner = %q, want engine-a", result.Record.Owner)
	}
	if result.Record.LeaseExpiresAt == nil || !result.Record.LeaseExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease expiry = %v, want %v", result.Record.LeaseExpiresAt, now.Add(time.Minute))
	}
}

func TestEffectClaim_SameOwnerRefreshes(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	claim := EffectClaim{
		EffectID: makeID("create_pr", effectPosStepAction),
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	}
	if _, err := exec.ClaimEffect(claim); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	result, err := exec.ClaimEffect(EffectClaim{
		EffectID: claim.EffectID,
		Owner:    "engine-a",
		LeaseTTL: 2 * time.Minute,
		Now:      now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("refresh claim: %v", err)
	}
	if !result.Acquired || !result.Refreshed {
		t.Fatalf("claim result = %+v, want refreshed lease", result)
	}
	if result.Record.LeaseExpiresAt == nil || !result.Record.LeaseExpiresAt.Equal(now.Add(150*time.Second)) {
		t.Fatalf("lease expiry = %v, want %v", result.Record.LeaseExpiresAt, now.Add(150*time.Second))
	}
}

func TestEffectClaim_ActiveOwnerConflict(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	claimID := makeID("create_pr", effectPosStepAction)
	if _, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	}); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	result, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-b",
		LeaseTTL: time.Minute,
		Now:      now.Add(30 * time.Second),
	})
	if !errors.Is(err, ErrEffectClaimConflict) {
		t.Fatalf("ClaimEffect err = %v, want %v", err, ErrEffectClaimConflict)
	}
	if result.Record.Owner != "engine-a" {
		t.Fatalf("owner = %q, want engine-a", result.Record.Owner)
	}
}

func TestEffectClaim_ExpiredOwnerReclaim(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	claimID := makeID("create_pr", effectPosStepAction)
	if _, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	}); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	result, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-b",
		LeaseTTL: time.Minute,
		Now:      now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Record.Owner != "engine-b" {
		t.Fatalf("owner = %q, want engine-b", result.Record.Owner)
	}
}

func TestEffectClaim_StaleOwnerCannotCompleteAfterReclaim(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	claimID := makeID("create_pr", effectPosStepAction)
	if _, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	}); err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if _, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-b",
		LeaseTTL: time.Minute,
		Now:      now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	_, err := exec.CompleteEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now.Add(2*time.Minute + time.Second),
	})
	if !errors.Is(err, ErrEffectClaimLost) {
		t.Fatalf("CompleteEffect err = %v, want %v", err, ErrEffectClaimLost)
	}
}

func TestEffectClaim_CompletedEffectCannotReplay(t *testing.T) {
	exec := &Execution{}
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	claimID := makeID("create_pr", effectPosStepAction)
	claim := EffectClaim{
		EffectID: claimID,
		Owner:    "engine-a",
		LeaseTTL: time.Minute,
		Now:      now,
	}
	if _, err := exec.ClaimEffect(claim); err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if _, err := exec.CompleteEffect(claim); err != nil {
		t.Fatalf("complete: %v", err)
	}

	_, err := exec.ClaimEffect(EffectClaim{
		EffectID: claimID,
		Owner:    "engine-b",
		LeaseTTL: time.Minute,
		Now:      now.Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrEffectAlreadyComplete) {
		t.Fatalf("ClaimEffect err = %v, want %v", err, ErrEffectAlreadyComplete)
	}
}
