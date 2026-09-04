package tasksnapshot

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
)

// TestMain fails the whole package immediately if git is missing, rather
// than letting individual tests silently t.Skip — a stripped-down test
// environment should show up as a red CI run, not quietly reduced coverage.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/tasksnapshot tests require git on PATH:", err)
		os.Exit(1)
	}
	// Cut the operator's global/system git config out of the run, the way
	// internal/worktree and internal/project already do. Without it these
	// tests inherit whatever the machine sets — commit.gpgsign among it —
	// and pass or fail on the developer's configuration rather than on this
	// package's behaviour.
	//
	// This reaches the snapshotter's own commands only because BuildEnv now
	// forwards the GIT_CONFIG_* variables Isolate sets. It used to strip
	// every GIT_* name, so isolation stopped at the test process and each
	// git invocation still read the real global config.
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/tasksnapshot tests require an isolated git config:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
