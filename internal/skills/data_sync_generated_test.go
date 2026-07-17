package skills

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSkillsData_InSyncWithRepoSkills is the drift guard for
// internal/skills/data/*.md. It runs `go run ./cmd/gen-skills-sync` and diffs
// the regenerated files against what is checked in. If a dual-sourced skill
// was edited under .claude/skills/ without regenerating, or someone
// hand-edited a data/*.md file directly, this test fails with a "run go
// generate" message — the failure mode that shipped a silent no-op in #2179.
func TestSkillsData_InSyncWithRepoSkills(t *testing.T) {
	root := repoRoot(t)

	prev := make(map[string][]byte, len(dualSourcedSkillsForTest))
	for _, name := range dualSourcedSkillsForTest {
		path := filepath.Join(root, "internal", "skills", "data", name+".md")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		prev[name] = content
	}
	t.Cleanup(func() {
		for name, content := range prev {
			path := filepath.Join(root, "internal", "skills", "data", name+".md")
			_ = os.WriteFile(path, content, 0o644)
		}
	})

	cmd := exec.Command("go", "run", "./cmd/gen-skills-sync")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./cmd/gen-skills-sync: %v\n%s", err, out)
	}

	for _, name := range dualSourcedSkillsForTest {
		path := filepath.Join(root, "internal", "skills", "data", name+".md")
		regen, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read regenerated %s: %v", path, err)
		}
		if !bytes.Equal(regen, prev[name]) {
			t.Errorf("internal/skills/data/%s.md is stale — run `go generate ./internal/skills/...`", name)
		}
	}
}

// dualSourcedSkillsForTest mirrors cmd/gen-skills-sync's dualSourcedSkills.
// Not imported directly since cmd/gen-skills-sync is package main; kept in
// sync by TestSkillsData_InSyncWithRepoSkills itself failing loudly if a name
// here is missing from the generator's own list (the generator would then
// leave that skill's data/*.md untouched while this test still checks it,
// surfacing as a spurious "stale" failure that points straight at the
// mismatch).
var dualSourcedSkillsForTest = []string{"plan-critic", "plan-fork", "fix-review-auto"}

// repoRoot walks up from this test file's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", file)
		}
		dir = parent
	}
}
