package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderLaunchRequiresAdmission is the static backstop for the runtime
// choke point: production code may submit RunConfigs through Manager, but only
// RunContext may cross from a typed intent into the provider runner, and it
// must acquire admission first. It also prevents the retired capacity bypass
// from quietly returning at a scheduler/watchdog/recovery call site.
func TestProviderLaunchRequiresAdmission(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	managerRun, err := os.ReadFile(filepath.Join(repoRoot, "internal", "agent", "manager_run.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(managerRun)
	acquireAt := strings.Index(source, "m.acquireAttempt(ctx, intent)")
	launchAt := strings.Index(source, "m.startAgentRunner(ctx, a, cfg, prov, cancel)")
	if acquireAt < 0 || launchAt < 0 || acquireAt >= launchAt {
		t.Fatalf("RunContext admission/launch order drifted: acquire=%d launch=%d", acquireAt, launchAt)
	}
	if got := strings.Count(source, "m.startAgentRunner("); got != 1 {
		t.Fatalf("production provider-runner entrypoints = %d, want exactly 1", got)
	}

	err = filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), "IgnoreConcurrencyLimit: true") {
			t.Errorf("%s restores the forbidden admission bypass", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
