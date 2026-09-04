package agent

import (
	"os"
	"testing"
)

func TestSandboxEnforce_LinkedWorktreeGitOps(t *testing.T) {
	if os.Getenv("FAKE_SANDBOX_SMOKE_FAIL") == "1" {
		t.Fatal("simulated sandbox smoke failure")
	}
}
