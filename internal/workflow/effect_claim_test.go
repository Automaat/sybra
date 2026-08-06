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

// A provider park now outlives defaultEffectLeaseTTL by two orders of
// magnitude — a provider-stated reset instant lands three days out against a
// thirty-minute lease. Pin what that actually means for a claim held when the
// park began, since "the lease expires while the task is parked" reads like a
// defect and is in fact the recovery path: expiry is what lets the next
// dispatcher take the effect, whether it is this engine or a fresh one.
func TestEffectClaim_LeaseLapsedDuringLongParkStaysUsable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	id := makeID("implement", effectPosStepAction)
	// Codex hands out three-day windows; the lease is thirty minutes.
	afterPark := now.Add(60 * time.Hour)

	cases := []struct {
		name  string
		owner string
	}{
		{name: "same engine still running", owner: "engine-a"},
		{name: "fresh engine after a restart", owner: "engine-b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := &Execution{}
			if _, err := exec.ClaimEffect(EffectClaim{
				EffectID: id, Owner: "engine-a",
				LeaseTTL: defaultEffectLeaseTTL, Now: now,
			}); err != nil {
				t.Fatalf("initial claim: %v", err)
			}

			result, err := exec.ClaimEffect(EffectClaim{
				EffectID: id, Owner: tc.owner,
				LeaseTTL: defaultEffectLeaseTTL, Now: afterPark,
			})
			if err != nil {
				t.Fatalf("claim after a 60h park: %v", err)
			}
			if !result.Acquired {
				t.Fatalf("claim result = %+v, want acquired", result)
			}
			if result.Record.Owner != tc.owner {
				t.Errorf("owner = %q, want %q", result.Record.Owner, tc.owner)
			}
			if result.Record.LeaseExpiresAt == nil ||
				!result.Record.LeaseExpiresAt.Equal(afterPark.Add(defaultEffectLeaseTTL)) {
				t.Errorf("lease expiry = %v, want it re-issued from the claim time",
					result.Record.LeaseExpiresAt)
			}
			if result.Record.CompletedAt != nil {
				t.Error("a lapsed lease must not mark the effect complete")
			}
		})
	}
}

// The other half of the same question: a park must not let a second engine
// steal an effect the first is still actively working, which is what the
// lease is for. Only elapsed time distinguishes the two, so pin the boundary.
func TestEffectClaim_ActiveLeaseStillFencesDuringPark(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	id := makeID("implement", effectPosStepAction)

	exec := &Execution{}
	if _, err := exec.ClaimEffect(EffectClaim{
		EffectID: id, Owner: "engine-a",
		LeaseTTL: defaultEffectLeaseTTL, Now: now,
	}); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	_, err := exec.ClaimEffect(EffectClaim{
		EffectID: id, Owner: "engine-b",
		LeaseTTL: defaultEffectLeaseTTL, Now: now.Add(defaultEffectLeaseTTL - time.Second),
	})
	if !errors.Is(err, ErrEffectClaimConflict) {
		t.Fatalf("claim inside the lease err = %v, want %v", err, ErrEffectClaimConflict)
	}
}
