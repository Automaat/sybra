package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"testing"
)

var stepTypeConstPattern = regexp.MustCompile(`(?m)^\s*(Step[A-Za-z0-9_]+)\s+StepType\s*=\s*"([^"]+)"`)

func TestStepRegistryExhaustive(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	modelPath := filepath.Join(filepath.Dir(thisFile), "model.go")
	src, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatalf("read %s: %v", modelPath, err)
	}

	matches := stepTypeConstPattern.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatalf("no StepType constants found in %s", modelPath)
	}

	var modelTypes []StepType
	for _, m := range matches {
		modelTypes = append(modelTypes, StepType(m[2]))
	}
	slices.Sort(modelTypes)

	registryTypes := make([]StepType, 0, len(stepRegistry))
	for stepType, spec := range stepRegistry {
		if spec.async == stepAsyncNone && spec.sync == stepSyncNone {
			t.Fatalf("stepRegistry[%q] has no handler", stepType)
		}
		if spec.async != stepAsyncNone && spec.sync != stepSyncNone {
			t.Fatalf("stepRegistry[%q] has both async and sync handlers", stepType)
		}
		registryTypes = append(registryTypes, stepType)
	}
	slices.Sort(registryTypes)

	if !reflect.DeepEqual(modelTypes, registryTypes) {
		t.Fatalf("step registry mismatch\nmodel:    %v\nregistry: %v", modelTypes, registryTypes)
	}
}
