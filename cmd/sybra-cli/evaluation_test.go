package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/prompteval"
)

// fakeOfflineRunner lets the test control Score deterministically instead of
// shelling out to a live provider CLI.
type fakeOfflineRunner struct {
	score float64
	err   error
}

func (f fakeOfflineRunner) Run(_ context.Context, _ prompteval.Spec) (prompteval.Result, error) {
	if f.err != nil {
		return prompteval.Result{}, f.err
	}
	return prompteval.Result{Score: f.score}, nil
}
func (f fakeOfflineRunner) Available() bool { return true }
func (f fakeOfflineRunner) Name() string    { return "fake" }

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCmdEvaluationOfflineExitsNonzeroOnFail(t *testing.T) {
	dir := setupStore(t)
	old := newOfflineRunner
	defer func() { newOfflineRunner = old }()
	newOfflineRunner = func(config.OfflineEvalConfig) prompteval.OfflineRunner {
		return fakeOfflineRunner{score: 0}
	}

	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{
		{ID: "failing-variant", Prompt: "some prompt", Provider: "claude", Model: "opus"},
	})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "c1", Input: "hi", Assertions: []prompteval.Assertion{{Type: "contains", Value: "x"}}},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for a FAIL verdict, got 0. output: %s", output)
	}
}

func TestCmdEvaluationOfflineRunExitsZeroOnPass(t *testing.T) {
	dir := setupStore(t)
	old := newOfflineRunner
	defer func() { newOfflineRunner = old }()
	newOfflineRunner = func(config.OfflineEvalConfig) prompteval.OfflineRunner {
		return fakeOfflineRunner{score: 1}
	}

	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{
		{ID: "passing-variant", Prompt: "some prompt", Provider: "claude", Model: "opus"},
	})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "c1", Input: "hi", Assertions: []prompteval.Assertion{{Type: "contains", Value: "x"}}},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code != 0 {
		t.Fatalf("expected exit 0 for a PASS verdict, got %d. output: %s", code, output)
	}

	// The verdict must be persisted so `gate` can read it back.
	digest := prompteval.Digest([]byte("some prompt"))
	gateCode, gateOut := runCLI(t, "evaluation", "offline", "gate", "passing-variant", digest, "--json")
	if gateCode != 0 {
		t.Fatalf("gate on a PASS verdict should exit 0, got %d. output: %s", gateCode, gateOut)
	}
}

func TestCmdEvaluationOfflineRunRejectsEmptyVariants(t *testing.T) {
	dir := setupStore(t)
	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "c1", Input: "hi"},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for an empty --variants list (must be a usage error, not a vacuous pass). output: %s", output)
	}
}

func TestCmdEvaluationOfflineGateDeniesMissingVerdict(t *testing.T) {
	setupStore(t)
	code, output := runCLI(t, "evaluation", "offline", "gate", "no-such-variant", "no-such-digest", "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for a missing verdict (fail-closed default), got 0. output: %s", output)
	}
}
