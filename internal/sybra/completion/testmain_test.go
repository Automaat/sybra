package completion

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
)

// TestMain fails the whole package immediately if git is missing, rather
// than letting individual git-dependent tests fail confusingly with a raw
// "exec: git" error — a stripped-down test environment should show up as a
// red CI run, not quietly reduced coverage.
func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/completion tests require git on PATH:", err)
		return 1
	}
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/completion tests require an isolated git config:", err)
		return 1
	}
	defer cleanup()
	return m.Run()
}
