package sybra

import (
	"os"
	"path/filepath"
)

// init seeds SYBRA_HOME for the package's tests so config.HomeDir() points
// at a writable location.
//
// Pre-PR1 the skills-sync tests (now in internal/skillsync) had the side
// effect of writing CLAUDE.md/AGENTS.md to config.HomeDir(), which
// implicitly created the directory. Other tests (notably the planning
// service tests) rely on config.HomeDir() existing because
// agentAdapter.StartAgent uses it as the cwd for system-role agents.
// Without this seed, those tests fail on Linux CI where /home/runner/.sybra
// does not exist by default.
//
// init runs once at test-binary start regardless of build tags, so it
// applies to both the regular suite and the e2e suite without needing a
// (non-shareable) TestMain.
func init() {
	dir := os.Getenv("SYBRA_HOME")
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "sybra-test-home-*")
		if err != nil {
			return
		}
		_ = os.Setenv("SYBRA_HOME", dir)
	}
	// Pre-create the dir whether we just chose it or inherited it — tests
	// (e.g. svc_planning_test) need the path to exist for agent cwd.
	_ = os.MkdirAll(filepath.Join(dir, "tasks"), 0o755)
}
