package agent

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"go.uber.org/goleak"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
)

// TestMain enforces goroutine cleanliness across agent tests. The agent
// package launches stream readers, tmux poll loops, and headless process
// supervisors — every test must wire them to a context that cancels by
// teardown. A failure here usually points at a missing <-ctx.Done() branch
// or an unbuffered channel that never drains.
func TestMain(m *testing.M) {
	// The darwin sandbox tests re-exec this binary inside a restricted
	// profile to probe it from the inside. That child needs no git, and the
	// profile denies the temp directory Isolate would create, so setting up
	// isolation there fails the probe instead of the thing it measures.
	if os.Getenv(sandboxProbeChildEnv) == "1" {
		os.Exit(m.Run())
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/agent tests require git on PATH:", err)
		os.Exit(1)
	}
	// Cut the ambient git environment out of the run. The sandbox integration
	// tests build real repos through exec.Command, which inherits it, so an
	// ambient GIT_DIR sent `git init` somewhere else entirely and the clone
	// that followed failed with "does not appear to be a git repository" —
	// pointing at the fixture rather than at the environment that broke it.
	// Isolate also pins the config, so an operator's commit.gpgsign no longer
	// decides whether these tests can commit.
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/agent tests require an isolated git config:", err)
		os.Exit(1)
	}
	// goleak.VerifyTestMain exits rather than returning, so cleanup is run by
	// wrapping m instead of deferring it — a defer here would never fire.
	goleak.VerifyTestMain(cleanupAfterRun{m: m, cleanup: cleanup},
		goleak.IgnoreTopFunction("runtime.cgocall"),
		// The default http.Transport keeps idle connections in a background
		// goroutine; harmless and process-global.
		goleak.IgnoreAnyFunction("net/http.(*Transport).dialConnFor"),
	)
}

// sandboxProbeChildEnv marks the re-exec of this test binary that runs
// inside a sandbox profile. Kept beside TestMain because that is where it
// changes behaviour; procsandbox_darwin_integration_test.go sets it.
const sandboxProbeChildEnv = "SYBRA_TEST_FSTAT_STDERR"

// cleanupAfterRun adapts *testing.M so teardown runs after the suite but
// before goleak verifies and exits.
type cleanupAfterRun struct {
	m       *testing.M
	cleanup func()
}

func (c cleanupAfterRun) Run() int {
	code := c.m.Run()
	c.cleanup()
	return code
}
