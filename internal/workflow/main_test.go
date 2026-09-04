package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
	"go.uber.org/goleak"
)

// TestMain runs goleak after every test to catch goroutines left running
// past their controlling context. The workflow engine spawns background
// dispatch/advance goroutines; stragglers indicate missing context
// cancellation or unclosed channels.
func TestMain(m *testing.M) {
	// Workflow unit tests create throwaway repos/worktrees outside Sybra's git
	// object overlay; inheriting the sandbox's object-dir env makes those repos
	// look corrupt to plain `git diff/fetch/clone` subprocesses.
	_ = os.Unsetenv("GIT_OBJECT_DIRECTORY")
	_ = os.Unsetenv("GIT_ALTERNATE_OBJECT_DIRECTORIES")
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/workflow tests require git on PATH:", err)
		os.Exit(1)
	}
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/workflow tests require an isolated git config:", err)
		os.Exit(1)
	}
	defer cleanup()
	goleak.VerifyTestMain(m,
		// Wails v3 internals start a CGO finalizer goroutine on macOS that
		// never exits; safe to ignore in unit tests that don't touch Wails.
		goleak.IgnoreTopFunction("runtime.cgocall"),
	)
}
