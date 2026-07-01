package prompteval

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()
	// A fresh, non-preexisting directory exercises the os.MkdirAll-on-first-write path.
	store := New(filepath.Join(t.TempDir(), "fresh-install"))
	v := VariantVerdict{
		VariantID: "v1",
		Digest:    Digest([]byte("prompt bytes")),
		Status:    StatusPass,
		Score:     0.95,
		CostUSD:   0.01,
		LatencyMS: 1234,
		Runner:    "native",
	}
	if err := store.Write(v); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := store.Read(v.VariantID, v.Digest)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Status != v.Status || got.Score != v.Score || got.VariantID != v.VariantID || got.Digest != v.Digest {
		t.Fatalf("Read = %+v, want %+v", got, v)
	}
}

func TestStoreReadMissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	_, err := store.Read("nope", "alsonope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read missing: err = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsPathTraversal(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	badKeys := []string{"../escape", "..", ".", "a/b", "a\\b", "", "with space", "with;semicolon"}
	for _, k := range badKeys {
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			if err := store.Write(VariantVerdict{VariantID: k, Digest: "validdigest123"}); err == nil {
				t.Fatalf("Write accepted hostile variantID %q", k)
			}
			if err := store.Write(VariantVerdict{VariantID: "validvariant", Digest: k}); err == nil {
				t.Fatalf("Write accepted hostile digest %q", k)
			}
			if _, err := store.Read(k, "validdigest123"); err == nil {
				t.Fatalf("Read accepted hostile variantID %q", k)
			}
		})
	}
}

func TestStoreConcurrentWrite(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir())
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.Write(VariantVerdict{
				VariantID: "shared-variant",
				Digest:    Digest([]byte("prompt")),
				Status:    StatusPass,
				Score:     float64(i),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Write %d: %v", i, err)
		}
	}
	// The file must be intact JSON (atomic rename means no torn writes), even
	// though which writer's value "won" is a race by design.
	got, err := store.Read("shared-variant", Digest([]byte("prompt")))
	if err != nil {
		t.Fatalf("Read after concurrent writes: %v", err)
	}
	if got.VariantID != "shared-variant" {
		t.Fatalf("Read after concurrent writes returned corrupt data: %+v", got)
	}
}
