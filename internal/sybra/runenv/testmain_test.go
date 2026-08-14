package runenv

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/Automaat/sybra/internal/testutil/gitenv"
)

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/runenv tests require git on PATH:", err)
		os.Exit(1)
	}
	cleanup, err := gitenv.Isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/sybra/runenv tests require an isolated git config:", err)
		os.Exit(1)
	}
	defer cleanup()
	os.Exit(m.Run())
}
