package review

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
)

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/review tests require git on PATH:", err)
		return 1
	}
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/review tests require an isolated git config:", err)
		return 1
	}
	defer cleanup()
	return m.Run()
}
