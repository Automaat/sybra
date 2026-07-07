package tasksnapshot

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// TestMain fails the whole package immediately if git is missing, rather
// than letting individual tests silently t.Skip — a stripped-down test
// environment should show up as a red CI run, not quietly reduced coverage.
func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(os.Stderr, "internal/tasksnapshot tests require git on PATH:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
