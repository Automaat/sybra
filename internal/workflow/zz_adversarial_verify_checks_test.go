package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAdversarialVerifyChecks_HumanRequiresWhenCorruptInstallRepairFails
// exercises task f8ca18d6's failure mode end to end: node_modules is left
// corrupted by a killed `npm ci` (missing .bin), and the repair's own `npm
// ci` also fails (e.g. killed again, or offline). The still-broken install must
// never run through the verify command, but the gate must also fail closed so
// an implementation cannot bypass verification by corrupting node_modules.
func TestAdversarialVerifyChecks_HumanRequiresWhenCorruptInstallRepairFails(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	if err := os.WriteFile(filepath.Join(frontend, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(frontend, "package-lock.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake `npm` that always fails — simulates the repair's own `npm ci`
	// getting killed again under the same host memory pressure.
	binDir := t.TempDir()
	npmScript := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A verify command that would only pass on a repaired install — proving
	// it never actually ran against the still-corrupted node_modules.
	cmd := "test -d frontend/node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), nil, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
		panic("unreachable")
	}

	ti, _ := tasks.GetTask("t1")
	if out.Output != "flagged" {
		t.Fatalf("output = %q, want flagged", out.Output)
	}
	if ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", ti.Status)
	}
	if ti.StatusReason == "" {
		t.Fatal("expected a human-required status reason")
	}
}
