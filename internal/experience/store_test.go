package experience

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreProjectScopingIdempotencyOrderingAndLimit(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.maxPerProject = 3

	base := time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC)
	for _, rec := range []Record{
		{TaskID: "old", ProjectID: "owner/repo", CreatedAt: base.Add(-time.Hour), Title: "old"},
		{TaskID: "tie-b", ProjectID: "owner/repo", CreatedAt: base, Title: "b"},
		{TaskID: "tie-a", ProjectID: "owner/repo", CreatedAt: base, Title: "a"},
		{TaskID: "new", ProjectID: "owner/repo", CreatedAt: base.Add(time.Hour), Title: "new"},
	} {
		if err := store.Put("owner/repo", rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Put("other/repo", Record{TaskID: "other", ProjectID: "other/repo", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("owner/repo", Record{TaskID: "new", ProjectID: "owner/repo", CreatedAt: base.Add(2 * time.Hour), Title: "newer"}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"new", "tie-a", "tie-b"}
	if len(got) != len(wantIDs) {
		t.Fatalf("len(Query) = %d, want %d: %+v", len(got), len(wantIDs), got)
	}
	for i, want := range wantIDs {
		if got[i].TaskID != want {
			t.Fatalf("record %d = %q, want %q", i, got[i].TaskID, want)
		}
	}
	if got[0].Title != "newer" {
		t.Fatalf("idempotent overwrite title = %q, want newer", got[0].Title)
	}

	other, err := store.Query("other/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].TaskID != "other" {
		t.Fatalf("other project Query = %+v, want only other", other)
	}
}

func TestStoreLimitAndCorruptSkip(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Put("owner/repo", Record{TaskID: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "owner--repo", "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Query("owner/repo", 0); err != nil || len(got) != 0 {
		t.Fatalf("Query limit 0 = %+v, %v; want empty nil", got, err)
	}
	got, err := store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != "a" {
		t.Fatalf("Query with corrupt file = %+v, want only valid record", got)
	}
}

func TestStoreRejectsUnsafeIDsAndDelete(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../repo", "owner/../repo", "owner/repo/extra", "owner\\repo", "work-zzzz"} {
		if _, err := sanitizeProjectID(id); err == nil {
			t.Fatalf("sanitizeProjectID(%q) succeeded, want error", id)
		}
	}
	if got, err := sanitizeProjectID("owner/repo"); err != nil || got != "owner--repo" {
		t.Fatalf("sanitizeProjectID(owner/repo) = %q, %v; want owner--repo", got, err)
	}
	opaqueKey := "work-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, err := sanitizeProjectID(opaqueKey); err != nil || got != opaqueKey {
		t.Fatalf("sanitizeProjectID(opaque work key) = %q, %v; want same key", got, err)
	}
	if err := store.Put("owner/repo", Record{TaskID: "task-1", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(opaqueKey, Record{TaskID: "task-2", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("owner/repo"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Query after Delete = %+v, want empty", got)
	}
}
