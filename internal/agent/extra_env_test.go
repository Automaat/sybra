package agent

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestRunConfig_ExtraEnv_Field verifies the ExtraEnv field exists on RunConfig
// and is usable as a []string.
func TestRunConfig_ExtraEnv_Field(t *testing.T) {
	t.Parallel()
	cfg := RunConfig{
		TaskID:   "t1",
		ExtraEnv: []string{"SANDBOX_URL=http://localhost:1234", "KUBECONFIG=/tmp/kube"},
	}
	if len(cfg.ExtraEnv) != 2 {
		t.Fatalf("ExtraEnv len = %d, want 2", len(cfg.ExtraEnv))
	}
}

// TestRunConfig_ExtraEnv_Empty verifies that empty ExtraEnv is nil/zero.
func TestRunConfig_ExtraEnv_Empty(t *testing.T) {
	t.Parallel()
	cfg := RunConfig{TaskID: "t1"}
	if len(cfg.ExtraEnv) != 0 {
		t.Errorf("default ExtraEnv should be empty, got %v", cfg.ExtraEnv)
	}
}

// TestRunConfig_ExtraEnv_DoesNotClobberPATH verifies that injecting ExtraEnv
// via append(os.Environ(), extraEnv...) preserves PATH in the resulting slice.
// This mirrors the exact logic used in the runners.
func TestRunConfig_ExtraEnv_DoesNotClobberPATH(t *testing.T) {
	t.Parallel()
	extraEnv := []string{"SANDBOX_URL=http://localhost:9999"}
	merged := append(os.Environ(), extraEnv...)

	hasPATH := slices.ContainsFunc(merged, func(e string) bool {
		return strings.HasPrefix(e, "PATH=")
	})
	if !hasPATH {
		t.Error("PATH missing from merged env — ExtraEnv clobbered parent environment")
	}

	hasSandbox := slices.ContainsFunc(merged, func(e string) bool {
		return e == "SANDBOX_URL=http://localhost:9999"
	})
	if !hasSandbox {
		t.Error("SANDBOX_URL missing from merged env")
	}
}

// TestRunConfig_ExtraEnv_MultipleVars verifies that multiple ExtraEnv entries
// all appear in the merged slice.
func TestRunConfig_ExtraEnv_MultipleVars(t *testing.T) {
	t.Parallel()
	extraEnv := []string{
		"SANDBOX_URL=http://localhost:1111",
		"KUBECONFIG=/tmp/synapse-task/kubeconfig",
	}
	merged := append(os.Environ(), extraEnv...)

	for _, want := range extraEnv {
		if !slices.Contains(merged, want) {
			t.Errorf("merged env missing %q", want)
		}
	}
}
