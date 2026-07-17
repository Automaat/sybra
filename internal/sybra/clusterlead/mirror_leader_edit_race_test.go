package clusterlead

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/task"
)

// TestMirrorApplyRejectsLegitimateFollowerUpdateAfterLeaderLocalEdit probes
// #2203's acceptance criterion #4 — "The cluster mirror's own Merge→Put path
// is unaffected for legitimate follower updates (still trusts a genuinely
// newer follower timestamp verbatim)" — against a scenario the criterion
// does not anticipate: Merge's own staleness guard compares the follower's
// UpdatedAt against canonical.MirrorUpdatedAt (a separate leader-local
// bookkeeping clock, last set by Merge itself), not against the on-disk
// canonical.UpdatedAt that Store.Put's new #2203 guard actually checks.
// Those two clocks can diverge: any ordinary leader-side edit through the
// normal Manager.Update path (title, tags, anything) bumps canonical.UpdatedAt
// to wall-clock now() without touching MirrorUpdatedAt at all. If a
// genuinely-newer follower status update (newer than MirrorUpdatedAt, so
// Merge accepts it) arrives with an UpdatedAt that is nonetheless older than
// the leader's freshly-bumped canonical.UpdatedAt, Store.Put's guard rejects
// it and silently keeps the stale status — reproducing the exact class of
// bug #2203 exists to fix, through the fix's own new code path.
func TestMirrorApplyRejectsLegitimateFollowerUpdateAfterLeaderLocalEdit(t *testing.T) {
	cfg := leaderConfig("http://unused", []string{"owner/pet"})
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	mirror := NewMirror(cfg, mgr, roster, nil, time.Second)

	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, _, err := mgr.Put(task.Task{
		ID: "task-pet", Title: "leader title", Status: task.StatusTodo,
		ProjectID: "owner/pet", AssignedNode: "pet-box", CreatedAt: t0, UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}

	// Step 1: a first mirror apply, exactly like TestMirrorApplyConvergesAndDropsStale.
	// This sets both canonical.UpdatedAt and canonical.MirrorUpdatedAt to t0+1h,
	// so the two clocks start in sync.
	firstFollowerUpdate := t0.Add(time.Hour)
	first := task.Task{
		ID: "task-pet", Title: "follower title", Status: task.StatusBlocked,
		ProjectID: "attacker/x", AssignedNode: "elsewhere", UpdatedAt: firstFollowerUpdate,
	}
	if !mirror.applyFollowerTask("pet-box", first) {
		t.Fatal("first follower apply must succeed")
	}
	got, _ := mgr.Get("task-pet")
	if got.Status != task.StatusBlocked {
		t.Fatalf("setup: status = %q, want blocked", got.Status)
	}
	if got.MirrorUpdatedAt == nil || !got.MirrorUpdatedAt.Equal(firstFollowerUpdate) {
		t.Fatalf("setup: MirrorUpdatedAt = %v, want %v", got.MirrorUpdatedAt, firstFollowerUpdate)
	}

	// Step 2: an ordinary LEADER-side edit through the normal Update path
	// (nothing to do with the mirror) bumps canonical.UpdatedAt to real
	// wall-clock now() — which, since t0 is fixed in 2026-07-01, is always
	// far later than any t0-relative timestamp used below. MirrorUpdatedAt
	// is untouched by Update: it is a Merge-only field.
	newTitle := "leader renamed it locally"
	if _, err := mgr.Update("task-pet", task.Update{Title: &newTitle}); err != nil {
		t.Fatal(err)
	}
	afterEdit, _ := mgr.Get("task-pet")
	if afterEdit.Title != newTitle {
		t.Fatalf("setup: leader edit did not apply, title = %q", afterEdit.Title)
	}
	if !afterEdit.UpdatedAt.After(firstFollowerUpdate) {
		t.Fatalf("setup: leader edit did not bump UpdatedAt past %v, got %v", firstFollowerUpdate, afterEdit.UpdatedAt)
	}

	// Step 3: the follower reports genuine forward progress — its own
	// UpdatedAt (t0+90m) is newer than MirrorUpdatedAt (t0+1h), so Merge's
	// own staleness guard says this is a legitimate, non-stale update that
	// must apply.
	genuineFollowerUpdate := t0.Add(90 * time.Minute)
	if !genuineFollowerUpdate.After(firstFollowerUpdate) {
		t.Fatal("test setup invariant broken: genuine follower update must be after MirrorUpdatedAt")
	}
	progressed := task.Task{
		ID: "task-pet", Title: "follower title", Status: task.StatusInProgress,
		ProjectID: "attacker/x", AssignedNode: "elsewhere", UpdatedAt: genuineFollowerUpdate,
	}
	applied := mirror.applyFollowerTask("pet-box", progressed)

	got, _ = mgr.Get("task-pet")
	if !applied || got.Status != task.StatusInProgress {
		t.Fatalf("Merge accepted a genuinely-newer-than-MirrorUpdatedAt follower status change (in-progress), "+
			"but Store.Put's #2203 guard rejected it because canonical.UpdatedAt (bumped by an unrelated leader edit) "+
			"outran canonical.MirrorUpdatedAt — applied=%v, got status=%q (want in-progress). "+
			"This reproduces the #2203 symptom (stale status silently frozen on the leader) through the fix's own new code path.",
			applied, got.Status)
	}
}
