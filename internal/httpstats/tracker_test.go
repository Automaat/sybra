package httpstats

import (
	"testing"
	"time"
)

func TestTracker_SnapshotEmptyIsZero(t *testing.T) {
	tr := New()
	snap := tr.Snapshot(time.Now(), 15*time.Minute)
	if snap.Total != 0 || snap.Errors != 0 || snap.ErrorRate != 0 {
		t.Fatalf("want zero snapshot, got %+v", snap)
	}
}

func TestTracker_RecordAndSnapshot(t *testing.T) {
	tr := New()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	tr.Record(now, false)
	tr.Record(now, false)
	tr.Record(now, false)
	tr.Record(now, true)

	snap := tr.Snapshot(now, 15*time.Minute)
	if snap.Total != 4 {
		t.Fatalf("want total 4, got %d", snap.Total)
	}
	if snap.Errors != 1 {
		t.Fatalf("want errors 1, got %d", snap.Errors)
	}
	if snap.ErrorRate != 0.25 {
		t.Fatalf("want error rate 0.25, got %v", snap.ErrorRate)
	}
}

func TestTracker_SnapshotExcludesOutsideWindow(t *testing.T) {
	tr := New()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// Outside the 15m window.
	tr.Record(now.Add(-30*time.Minute), true)
	// Inside the window.
	tr.Record(now.Add(-5*time.Minute), false)

	snap := tr.Snapshot(now, 15*time.Minute)
	if snap.Total != 1 || snap.Errors != 0 {
		t.Fatalf("want only the in-window request counted, got %+v", snap)
	}
}

func TestTracker_SnapshotExcludesFutureBuckets(t *testing.T) {
	tr := New()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	tr.Record(now, false)
	// Recorded "in the future" relative to an earlier query time — must not
	// leak into a snapshot queried as of an earlier now (see the tracker
	// singleton sharing note on internal/metrics.RecordHTTPRequest).
	tr.Record(now.Add(time.Hour), true)

	snap := tr.Snapshot(now, 15*time.Minute)
	if snap.Total != 1 || snap.Errors != 0 {
		t.Fatalf("want the future bucket excluded, got %+v", snap)
	}
}

func TestTracker_EvictsOldBuckets(t *testing.T) {
	tr := New()
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	tr.Record(base, true)
	// Advance well past the retained window (maxBuckets*bucketWidth = 60m).
	later := base.Add(2 * time.Hour)
	tr.Record(later, false)

	snap := tr.Snapshot(later, time.Hour)
	if snap.Total != 1 || snap.Errors != 0 {
		t.Fatalf("want the stale bucket evicted, got %+v", snap)
	}
	if got := len(tr.buckets); got != 1 {
		t.Fatalf("want 1 retained bucket, got %d", got)
	}
}

func TestTracker_ConcurrentRecord(t *testing.T) {
	tr := New()
	now := time.Now()
	done := make(chan struct{})
	const goroutines = 20
	const perGoroutine = 50
	for range goroutines {
		go func() {
			defer func() { done <- struct{}{} }()
			for range perGoroutine {
				tr.Record(now, false)
			}
		}()
	}
	for range goroutines {
		<-done
	}
	snap := tr.Snapshot(now, time.Minute)
	if snap.Total != goroutines*perGoroutine {
		t.Fatalf("want %d recorded requests, got %d", goroutines*perGoroutine, snap.Total)
	}
}
