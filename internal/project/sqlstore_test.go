package project

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	yaml "gopkg.in/yaml.v3"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func projectFixture(t *testing.T, clonesDir string) []Project {
	t.Helper()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	out := []Project{
		{ID: "owner/beta", Owner: "owner", Repo: "beta", URL: "https://github.com/owner/beta",
			Type: ProjectTypePet, Status: ProjectStatusReady, CreatedAt: now, UpdatedAt: now},
		{ID: "owner/alpha", Owner: "owner", Repo: "alpha", URL: "https://github.com/owner/alpha",
			Type: ProjectTypePet, Status: ProjectStatusReady, CreatedAt: now, UpdatedAt: now},
	}
	for i := range out {
		clone := filepath.Join(clonesDir, out[i].Repo+".git")
		if err := os.MkdirAll(clone, 0o755); err != nil {
			t.Fatalf("mkdir clone: %v", err)
		}
		out[i].ClonePath = clone
	}
	return out
}

func ids(list []Project) []string {
	out := make([]string, 0, len(list))
	for i := range list {
		out = append(out, list[i].ID)
	}
	return out
}

// TestSQLStore_RoundTripsAndListsStably pins that a record survives and that
// the project list is in one order, which the directory listing gave before.
func TestSQLStore_RoundTripsAndListsStably(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		for _, p := range projectFixture(t, t.TempDir()) {
			if err := store.Write(p); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		got, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 || got[0].ID != "owner/alpha" {
			t.Fatalf("List returned %v, want alpha first", ids(got))
		}

		one, err := store.Read("owner/beta")
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if one.Repo != "beta" {
			t.Errorf("read repo = %q, want beta", one.Repo)
		}

		if err := store.Delete("owner/beta"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.Read("owner/beta"); err == nil {
			t.Error("a deleted project still reads back")
		}
	})
}

// TestSQLStore_RawTypeDoesNotDefault pins the confidentiality guard: a record
// with no type must not read as pet, because that routes a work project to an
// untrusted follower. The defaulting read is checked alongside it so the two
// paths cannot drift into agreeing.
func TestSQLStore_RawTypeDoesNotDefault(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		backend, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		store, err := NewStoreWith(t.TempDir(), t.TempDir(), backend)
		if err != nil {
			t.Fatalf("NewStoreWith: %v", err)
		}
		if err := backend.Write(Project{ID: "owner/untyped", Owner: "owner", Repo: "untyped"}); err != nil {
			t.Fatalf("Write: %v", err)
		}

		raw, err := store.RawType("owner/untyped")
		if err != nil {
			t.Fatalf("RawType: %v", err)
		}
		if raw != "" {
			t.Fatalf("RawType = %q, want the empty type it was stored with", raw)
		}

		got, err := store.Get("owner/untyped")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Type != ProjectTypePet {
			t.Errorf("Get type = %q, want the pet default", got.Type)
		}
	})
}

// TestImport_KeepsClonesMatchedAndReportsMissingOnes is the issue's "existing
// project files import once, and their clones are matched to the imported
// records" plus "a record whose clone is missing is reported rather than
// silently used".
func TestImport_KeepsClonesMatchedAndReportsMissingOnes(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, d *db.DB) {
		t.Helper()
		dir := t.TempDir()
		clones := t.TempDir()
		fixture := projectFixture(t, clones)
		// A project whose clone was removed underneath it, which is what a
		// crash between writing the record and finishing the clone leaves.
		fixture = append(fixture, Project{
			ID: "owner/stranded", Owner: "owner", Repo: "stranded",
			Type: ProjectTypePet, Status: ProjectStatusReady,
			ClonePath: filepath.Join(clones, "gone.git"),
		})
		for _, p := range fixture {
			data, err := yaml.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			name := p.Owner + "--" + p.Repo + ".yaml"
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
		}

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		for range 2 {
			if err := Import(t.Context(), d, dir, "home-a", logger); err != nil {
				t.Fatalf("import: %v", err)
			}
		}

		store, err := NewSQLStore(d)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		got, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("after two imports the board holds %d projects, want 3: %v", len(got), ids(got))
		}
		for i := range got {
			if got[i].ClonePath == "" {
				t.Errorf("%s imported without its clone path", got[i].ID)
			}
		}
		if !bytes.Contains(buf.Bytes(), []byte("project.import.clone_missing")) {
			t.Fatalf("a project whose clone is gone was imported silently; log was %q", buf.String())
		}

		for _, p := range fixture[:2] {
			if _, err := os.Stat(p.ClonePath); err != nil {
				t.Errorf("import disturbed the clone for %s: %v", p.ID, err)
			}
		}
	})
}
