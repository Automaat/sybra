// Command gen-skills-sync regenerates the embedded fallback copies under
// internal/skills/data/ from the repo's .claude/skills/<name>/SKILL.md for
// every dual-sourced skill. Run via `go generate ./internal/skills/...`
// (the directive lives in internal/skills/embed.go).
//
// internal/skillsync disables the embedded bundle whenever the repo checkout
// is present (the case on every real deployment, including the server) and
// syncs .claude/skills over it instead — so the embedded copy is only ever
// read in an embedded-only deployment (e.g. Docker with no repo checkout).
// Before this generator existed, the two copies were hand-maintained
// separately and drifted silently: #2179 updated .claude/skills/plan-critic
// without touching internal/skills/data/plan-critic.md, and the change never
// reached a running agent in any real deployment. Regenerating from the repo
// copy keeps .claude/skills as the single edited source; the drift guard in
// internal/skills/data_sync_generated_test.go fails loudly if this generator
// was not re-run after an edit.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// dualSourcedSkills lists every skill maintained in both .claude/skills/ and
// internal/skills/data/. Add a name here when a new skill needs an embedded
// fallback for deployments without a repo checkout.
var dualSourcedSkills = []string{"plan-critic", "plan-fork", "fix-review-auto"}

func main() {
	root, err := findModuleRoot(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root string) error {
	for _, name := range dualSourcedSkills {
		src := filepath.Join(root, ".claude", "skills", name, "SKILL.md")
		dst := filepath.Join(root, "internal", "skills", "data", name+".md")
		content, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// findModuleRoot walks up from start looking for go.mod.
func findModuleRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found starting from %s", start)
		}
		dir = parent
	}
}
