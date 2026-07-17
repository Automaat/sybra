package task

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestAdversarialStorePutTOCTOU pins that Store.Put's #2203 stale-status
// guard is safe for concurrent use within a process, per the Store type's
// own doc comment. Put now locks via s.lockTask around its read-then-write
// span — without it, two concurrent Puts for the same ID could interleave
// and produce exactly the stale clobber the #2203 guard exists to prevent.
//
// Scenario: on disk starts at status=blocked/T0. Two concurrent Puts land:
//   - B: a legitimate, genuinely-advancing update (status=inprogress, T5)
//   - A: a stale re-push carrying the *original* T0 (status=todo, T0) — the
//     exact #2203 shape that must be rejected once B's advance is visible.
//
// Under any serialized (locked) execution order, the final state is
// deterministically inprogress/T5 (verified by hand: A-then-B ends at T5
// because B genuinely advances past A's no-op; B-then-A ends at T5 because
// A's guard triggers against the now-current inprogress/T5 disk state and
// rejects the stale todo/T0). If the observed final state after concurrent,
// unsynchronized execution is ever something else (e.g. blocked/T0 or
// todo/T0), that proves the read-then-write span raced: A must have read
// the pre-B state before B wrote, then written after B — the very stale
// clobber the #2203 guard exists to prevent.
func TestAdversarialStorePutTOCTOU(t *testing.T) {
	t0 := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	t5 := t0.Add(5 * time.Minute)

	const trials = 300
	violations := 0
	var samples []Task

	for range trials {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(Task{
			ID: "task-race", Title: "t", Status: StatusBlocked,
			CreatedAt: t0, UpdatedAt: t0,
		}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = store.Put(Task{
				ID: "task-race", Title: "t", Status: StatusTodo,
				CreatedAt: t0, UpdatedAt: t0, // stale: identical to original on-disk UpdatedAt
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = store.Put(Task{
				ID: "task-race", Title: "t", Status: StatusInProgress,
				CreatedAt: t0, UpdatedAt: t5, // genuinely advancing
			})
		}()
		close(start)
		wg.Wait()

		got, err := store.Get("task-race")
		if err != nil {
			t.Fatalf("Get after race: %v", err)
		}
		if got.Status != StatusInProgress || !got.UpdatedAt.Equal(t5) {
			violations++
			if len(samples) < 5 {
				samples = append(samples, got)
			}
		}
	}

	if violations > 0 {
		t.Fatalf("TOCTOU violation: %d/%d trials did not converge to the only two valid serialized outcomes (status=in-progress, updated_at=%v); samples: %+v",
			violations, trials, t5, samples)
	}
	t.Logf("0/%d trials violated serialized-outcome invariant", trials)
}

// TestAdversarialManagerPutSerializesSameID exercises the actual production
// path (Manager.Put, which every real caller — TaskService.AssignTask,
// clusterlead.Mirror, clusterlead.Assigner, clusterlead.Reassign — goes
// through) with the same race shape as above, to confirm Manager's
// per-ID in-process mutex (lockFor) does what Store.Put itself does not:
// serialize the read-decide-write span.
func TestAdversarialManagerPutSerializesSameID(t *testing.T) {
	t0 := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	t5 := t0.Add(5 * time.Minute)

	const trials = 300
	violations := 0

	for range trials {
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		mgr := NewManager(store, NoopEmitter())
		if _, err := store.Put(Task{
			ID: "task-race", Title: "t", Status: StatusBlocked,
			CreatedAt: t0, UpdatedAt: t0,
		}); err != nil {
			t.Fatalf("seed Put: %v", err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = mgr.Put(Task{
				ID: "task-race", Title: "t", Status: StatusTodo,
				CreatedAt: t0, UpdatedAt: t0,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = mgr.Put(Task{
				ID: "task-race", Title: "t", Status: StatusInProgress,
				CreatedAt: t0, UpdatedAt: t5,
			})
		}()
		close(start)
		wg.Wait()

		got, err := store.Get("task-race")
		if err != nil {
			t.Fatalf("Get after race: %v", err)
		}
		if got.Status != StatusInProgress || !got.UpdatedAt.Equal(t5) {
			violations++
		}
	}

	if violations > 0 {
		t.Errorf("Manager.Put allowed %d/%d TOCTOU violations despite lockFor", violations, trials)
	} else {
		t.Logf("Manager.Put: 0/%d violations — lockFor serializes the race", trials)
	}
}

// TestAdversarialPutFirstWritePassesThroughUntouched confirms the #2203
// guard's s.read(t.ID) branch for a brand-new ID (os.ErrNotExist) does not
// accidentally trip the guard and clobber a legitimate first-ever write.
func TestAdversarialPutFirstWritePassesThroughUntouched(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	saved, err := store.Put(Task{
		ID: "task-new", Title: "brand new", Status: StatusInProgress,
		CreatedAt: ts, UpdatedAt: ts,
	})
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if saved.Status != StatusInProgress {
		t.Fatalf("status = %q, want in-progress (first write must not be guarded)", saved.Status)
	}
	if !saved.UpdatedAt.Equal(ts) {
		t.Fatalf("UpdatedAt = %v, want verbatim %v", saved.UpdatedAt, ts)
	}
}

// TestAdversarialPutZeroOnDiskUpdatedAt probes whether !After is
// well-defined when the existing on-disk UpdatedAt is a zero time.Time
// (e.g. a legacy/hand-crafted record). A non-zero incoming UpdatedAt should
// count as strictly advancing past zero, so the status change must go
// through rather than being spuriously rejected.
func TestAdversarialPutZeroOnDiskUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Hand-write a task with a zero UpdatedAt directly, bypassing Put's
	// zero-defaulting, to simulate a corrupted/legacy on-disk record.
	raw := Task{ID: "task-zero", Title: "t", Status: StatusBlocked}
	data, err := marshalTask(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/task-zero.md", data, 0o644); err != nil {
		t.Fatal(err)
	}

	newTS := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	saved, err := store.Put(Task{
		ID: "task-zero", Title: "t", Status: StatusTodo,
		CreatedAt: newTS, UpdatedAt: newTS,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if saved.Status != StatusTodo {
		t.Fatalf("status = %q, want todo — a non-zero UpdatedAt must count as advancing past a zero on-disk UpdatedAt", saved.Status)
	}
	if !saved.UpdatedAt.Equal(newTS) {
		t.Fatalf("UpdatedAt = %v, want verbatim %v", saved.UpdatedAt, newTS)
	}
}

// TestAdversarialFlappingMirrorConverges simulates a flapping mirror
// re-pushing the same stale snapshot alongside genuine forward progress in
// a tight sequential loop, and asserts the final state is the last
// genuinely-advancing status rather than oscillating back to something stale.
func TestAdversarialFlappingMirrorConverges(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 14, 19, 3, 4, 0, time.UTC)
	if _, err := store.Put(Task{ID: "task-flap", Title: "t", Status: StatusBlocked, CreatedAt: base, UpdatedAt: base}); err != nil {
		t.Fatal(err)
	}

	toggle := func(s Status) Status {
		if s == StatusBlocked {
			return StatusInProgress
		}
		return StatusBlocked
	}

	advance := base
	curStatus := StatusBlocked
	for i := range 50 {
		other := toggle(curStatus)

		// Flapping stale re-push carrying the *other* status at the
		// original base timestamp — must be rejected, whatever curStatus
		// currently is.
		if _, err := store.Put(Task{ID: "task-flap", Title: "t", Status: other, CreatedAt: base, UpdatedAt: base}); err != nil {
			t.Fatalf("flap Put %d: %v", i, err)
		}
		got, err := store.Get("task-flap")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != curStatus || !got.UpdatedAt.Equal(advance) {
			t.Fatalf("iter %d: status regressed to %q/%v from a stale re-push, want %q/%v", i, got.Status, got.UpdatedAt, curStatus, advance)
		}

		// Genuine forward progress: flip to the other status with an
		// advancing timestamp.
		advance = advance.Add(time.Minute)
		if _, err := store.Put(Task{ID: "task-flap", Title: "t", Status: other, CreatedAt: base, UpdatedAt: advance}); err != nil {
			t.Fatalf("advance Put %d: %v", i, err)
		}
		curStatus = other
		got, err = store.Get("task-flap")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != curStatus || !got.UpdatedAt.Equal(advance) {
			t.Fatalf("iter %d: expected %q/%v after genuine advance, got %q/%v", i, curStatus, advance, got.Status, got.UpdatedAt)
		}

		// Re-flap with the *original* base snapshot again, carrying the
		// now-stale opposite status — must still be rejected now that disk
		// is ahead at `advance`.
		if _, err := store.Put(Task{ID: "task-flap", Title: "t", Status: toggle(curStatus), CreatedAt: base, UpdatedAt: base}); err != nil {
			t.Fatalf("re-flap Put %d: %v", i, err)
		}
		got, err = store.Get("task-flap")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != curStatus || !got.UpdatedAt.Equal(advance) {
			t.Fatalf("iter %d: stale re-flap corrupted state, got %q/%v want %q/%v", i, got.Status, got.UpdatedAt, curStatus, advance)
		}
	}
}
