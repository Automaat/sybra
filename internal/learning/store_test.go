package learning

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mkDigest(t *testing.T, since, until time.Time, reportDigest string) Digest {
	t.Helper()
	return Digest{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   until,
		Since:         since,
		Until:         until,
		ReportDigest:  reportDigest,
		Worked:        []string{"tests pass"},
		NotWorked:     []string{"flaky ci"},
		NextBets:      []string{"add retries"},
	}
}

func TestPutListGet(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d1 := mkDigest(t, base, base.AddDate(0, 0, 7), "digest-1")
	d2 := mkDigest(t, base.AddDate(0, 0, 7), base.AddDate(0, 0, 14), "digest-2")

	stored, err := store.Put(d1)
	if err != nil || !stored {
		t.Fatalf("Put d1: stored=%v err=%v", stored, err)
	}
	stored, err = store.Put(d2)
	if err != nil || !stored {
		t.Fatalf("Put d2: stored=%v err=%v", stored, err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 digests, got %d", len(list))
	}
	if list[0].ReportDigest != "digest-2" {
		t.Errorf("expected newest-first, got %q first", list[0].ReportDigest)
	}

	got, ok, err := store.Get(d1.Key())
	if err != nil || !ok {
		t.Fatalf("Get d1: ok=%v err=%v", ok, err)
	}
	if got.ReportDigest != "digest-1" {
		t.Errorf("Get returned wrong digest: %q", got.ReportDigest)
	}

	_, ok, err = store.Get(Key{Since: base, Until: base.AddDate(0, 0, 99), ReportDigest: "missing"})
	if err != nil {
		t.Fatalf("Get missing: unexpected err %v", err)
	}
	if ok {
		t.Error("Get missing: expected ok=false")
	}
}

func TestDuplicateSuppression(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d := mkDigest(t, base, base.AddDate(0, 0, 7), "digest-1")

	stored, err := store.Put(d)
	if err != nil || !stored {
		t.Fatalf("first Put: stored=%v err=%v", stored, err)
	}
	stored, err = store.Put(d)
	if err != nil {
		t.Fatalf("second Put: unexpected err %v", err)
	}
	if stored {
		t.Error("second Put with same key should report stored=false")
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 persisted digest after duplicate Put, got %d", len(list))
	}
}

func TestConcurrentDuplicatePut(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d := mkDigest(t, base, base.AddDate(0, 0, 7), "digest-1")

	const goroutines = 16
	var wg sync.WaitGroup
	storedCount := make([]bool, goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			storedCount[idx], errs[idx] = store.Put(d)
		}(i)
	}
	wg.Wait()

	trueCount := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected err %v", i, err)
		}
		if storedCount[i] {
			trueCount++
		}
	}
	if trueCount != 1 {
		t.Errorf("expected exactly 1 goroutine to observe stored=true, got %d", trueCount)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 persisted file, got %d", len(list))
	}
}

func TestMalformedRowsSkipped(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d := mkDigest(t, base, base.AddDate(0, 0, 7), "digest-good")
	if _, err := store.Put(d); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: unexpected err %v (malformed rows must be skipped, not fail the call)", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 valid digest (corrupt file skipped), got %d", len(list))
	}
	if list[0].ReportDigest != "digest-good" {
		t.Errorf("unexpected surviving digest: %q", list[0].ReportDigest)
	}
}

func TestRetentionCap(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.max = 3

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		since := base.AddDate(0, 0, i*7)
		until := since.AddDate(0, 0, 7)
		d := mkDigest(t, since, until, fmt.Sprintf("digest-%d", i))
		if _, err := store.Put(d); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected retention cap of 3, got %d", len(list))
	}
	// Newest-first: the 3 kept must be the 3 most recently generated.
	want := []string{"digest-4", "digest-3", "digest-2"}
	for i, d := range list {
		if d.ReportDigest != want[i] {
			t.Errorf("index %d: got %q, want %q", i, d.ReportDigest, want[i])
		}
	}
}

func TestLatestJSONDisposable(t *testing.T) {
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	d1 := mkDigest(t, base, base.AddDate(0, 0, 7), "digest-1")
	d2 := mkDigest(t, base.AddDate(0, 0, 7), base.AddDate(0, 0, 14), "digest-2")
	if _, err := store.Put(d1); err != nil {
		t.Fatalf("Put d1: %v", err)
	}
	if _, err := store.Put(d2); err != nil {
		t.Fatalf("Put d2: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, latestFileName)); err != nil {
		t.Fatalf("remove latest.json: %v", err)
	}

	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List correctness must not depend on latest.json, got %d entries", len(list))
	}

	latest, ok, err := store.Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !ok {
		t.Fatal("Latest: expected ok=true after latest.json removal (fallback to List)")
	}
	if latest.ReportDigest != "digest-2" {
		t.Errorf("Latest fallback returned wrong digest: %q", latest.ReportDigest)
	}
}
