package project

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
//
// It also cuts the developer's global/system git config out of the run: an
// ambient `insteadOf` URL rewrite otherwise reshapes remotes behind the tests'
// backs and makes them assert against code paths that never execute.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/project tests require git on PATH:", err)
		os.Exit(1)
	}
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/project tests require an isolated git config:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
