package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAdversarialVerifyChecks_DoesNotHumanRequireWhenCorruptInstallRepairFails
// exercises task f8ca18d6's failure mode end to end: node_modules is left
// corrupted by a killed `npm ci` (missing .bin), and the repair's own `npm
// ci` also fails (e.g. killed again, or offline). The still-broken install
// must never be run through the verify command and misattributed to the
// diff as a human-required implementation failure.
func TestAdversarialVerifyChecks_DoesNotHumanRequireWhenCorruptInstallRepairFails(t *testing.T) {
	wt := makeBaseRepo(t, map[string]string{"README.md": "init\n"})
	frontend := filepath.Join(wt, "frontend")
	nm := filepath.Join(frontend, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "vite"), 0o755); err != nil {
		t.Fatal(err)
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
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// A verify command that would only pass on a repaired install — proving
	// it never actually ran against the still-corrupted node_modules.
	cmd := "test -d frontend/node_modules/.bin"
	engine, tasks := newVerifyChecksEngine(t, wt, []string{cmd})
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})

	out, err := engine.execVerifyChecks("t1", newVerifyChecksStep(), TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatalf("corrupted node_modules with failed repair still routed to human-required; output=%q reason=%q",
			out.Output, ti.StatusReason)
	}
	if ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged (in-progress)", ti.Status)
	}
}
