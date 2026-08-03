package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
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
		if spec.async == nil && spec.sync == nil {
			t.Fatalf("stepRegistry[%q] has no handler", stepType)
		}
		if spec.async != nil && spec.sync != nil {
			t.Fatalf("stepRegistry[%q] has both async and sync handlers", stepType)
		}
		registryTypes = append(registryTypes, stepType)
	}
	slices.Sort(registryTypes)

	if !reflect.DeepEqual(modelTypes, registryTypes) {
		t.Fatalf("step registry mismatch\nmodel:    %v\nregistry: %v", modelTypes, registryTypes)
	}
}

func TestStepRegistrySyncHandlerDispatchesFromRegistryOnly(t *testing.T) {
	const probeType StepType = "noop_probe"
	const probeTaskID = "task-probe"

	old, existed := stepRegistry[probeType]
	stepRegistry[probeType] = stepSpec{
		sync: func(_ *Engine, taskID string, step *Step, _ *Execution, _ TemplateContext, _ TaskInfo) (StepOutput, error) {
			return StepOutput{
				StepID: step.ID,
				Status: "completed",
				Output: taskID,
			}, nil
		},
		reducer: stepReducerDispatch,
	}
	t.Cleanup(func() {
		if existed {
			stepRegistry[probeType] = old
			return
		}
		delete(stepRegistry, probeType)
	})

	e := &Engine{}
	step := &Step{ID: "s1", Type: probeType}
	out, err := e.execSyncStep(probeTaskID, step, &Execution{}, TemplateContext{}, TaskInfo{})
	if err != nil {
		t.Fatalf("execSyncStep: %v", err)
	}
	if out.StepID != step.ID || out.Status != "completed" || out.Output != probeTaskID {
		t.Fatalf("unexpected output: %+v", out)
	}
}

func TestEngineAdvanceHasNoHandlerTagDispatchSwitches(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	enginePath := filepath.Join(filepath.Dir(thisFile), "engine_advance.go")
	src, err := os.ReadFile(enginePath)
	if err != nil {
		t.Fatalf("read %s: %v", enginePath, err)
	}

	text := string(src)
	if strings.Contains(text, "switch spec.sync") {
		t.Fatalf("%s still switches on spec.sync", enginePath)
	}
	if strings.Contains(text, "switch spec.async") {
		t.Fatalf("%s still switches on spec.async", enginePath)
	}
}
