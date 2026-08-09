package sybra

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestOpenWorkflowStore_UnusableDirYieldsANilRepository pins the typed-nil trap.
//
// openWorkflowStore returns an interface. Handing back a nil *Store on the
// error path makes a NON-nil interface, so every `if store == nil` guard
// downstream passes it through and the first call — SyncBuiltins' List — panics
// on the nil receiver, taking startup with it.
func TestOpenWorkflowStore_UnusableDirYieldsANilRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)
	// A regular file where the workflows directory belongs, which is what an
	// operator's stray file or a read-only home produces.
	if err := os.WriteFile(filepath.Join(home, "workflows"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	a := NewApp(discardLogger(), &slog.LevelVar{}, testConfig(t))
	store, err := a.openWorkflowStore(t.Context())
	if err == nil {
		t.Fatal("opening a store over a regular file succeeded")
	}
	if store != nil {
		t.Fatalf("returned a non-nil repository (%T) alongside an error; a nil guard cannot see it", store)
	}

	// The engine must survive being handed that nil rather than panicking.
	a.initWorkflowEngine(nil)
}

// TestDatabaseBackend_ImportsThenSeeds runs startup's store wiring against a
// real sqlite backend, which nothing in this package did before.
//
// It covers the ordering the change depends on: the workflow import runs first,
// so an operator's edited definition survives, and the builtins seed on top
// rather than being overwritten by an import that arrived late.
func TestDatabaseBackend_ImportsThenSeeds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	workflowsDir := filepath.Join(home, "workflows")
	if err := os.MkdirAll(workflowsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// An operator's own workflow, which no builtin will ever seed over.
	const custom = "id: operator-only\nname: operator's own\nsteps:\n  - id: s\n    type: set_status\n    config:\n      status: todo\n"
	if err := os.WriteFile(filepath.Join(workflowsDir, "operator-only.yaml"), []byte(custom), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	cfg := testConfig(t)
	cfg.Database = config.DatabaseConfig{Backend: config.DBBackendSQLite, DSN: filepath.Join(home, "sybra.db")}

	a := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if err := a.initDatabase(t.Context()); err != nil {
		t.Fatalf("initDatabase: %v", err)
	}
	t.Cleanup(func() {
		if a.database != nil {
			_ = a.database.Close()
		}
	})
	if a.database == nil {
		t.Fatal("sqlite backend configured but no database opened")
	}

	store, err := a.openWorkflowStore(t.Context())
	if err != nil {
		t.Fatalf("openWorkflowStore: %v", err)
	}
	if _, ok := store.(*workflow.SQLStore); !ok {
		t.Fatalf("configured backend gave %T, want the database-backed store", store)
	}
	if err := workflow.SyncBuiltins(store); err != nil {
		t.Fatalf("sync builtins: %v", err)
	}

	got, err := store.Get("operator-only")
	if err != nil {
		t.Fatalf("the operator's own workflow did not survive import + seed: %v", err)
	}
	if got.Name != "operator's own" {
		t.Errorf("workflow name = %q, want the operator's", got.Name)
	}

	defs, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(defs) < 2 {
		t.Fatalf("listed %d definitions; the builtins did not seed alongside the imported one", len(defs))
	}
}

// TestDatabaseBackend_ExperienceUsesTheBackend pins that the advisory store
// actually follows the configured backend rather than quietly staying on files.
func TestDatabaseBackend_ExperienceUsesTheBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	cfg := testConfig(t)
	cfg.Database = config.DatabaseConfig{Backend: config.DBBackendSQLite, DSN: filepath.Join(home, "sybra.db")}

	a := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if err := a.initDatabase(t.Context()); err != nil {
		t.Fatalf("initDatabase: %v", err)
	}
	t.Cleanup(func() {
		if a.database != nil {
			_ = a.database.Close()
		}
	})
	a.initExperience(t.Context())
	if _, ok := a.experience.(*experience.SQLStore); !ok {
		t.Fatalf("advisory memory is %T, want the database-backed store", a.experience)
	}
}
