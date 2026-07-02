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

// fakeOfflineRunner lets the test control Score and Passed deterministically
// instead of shelling out to a live provider CLI. When forcePassed is nil,
// Passed defaults to score >= 1 so existing score-only tests keep working.
type fakeOfflineRunner struct {
	score       float64
	forcePassed *bool
	err         error
}

func (f fakeOfflineRunner) Run(_ context.Context, _ prompteval.Spec) (prompteval.Result, error) {
	if f.err != nil {
		return prompteval.Result{}, f.err
	}
	passed := f.score >= 1
	if f.forcePassed != nil {
		passed = *f.forcePassed
	}
	return prompteval.Result{Score: f.score, Passed: passed}, nil
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

// TestCmdEvaluationOfflineRunExitsNonzeroOnUnavailable guards against a
// broken eval setup (e.g. promptfoo binary missing) reading as green in CI:
// under the default fail-closed UnavailablePolicy, a runner error must exit
// nonzero even though no verdict is StatusFail.
func TestCmdEvaluationOfflineRunExitsNonzeroOnUnavailable(t *testing.T) {
	dir := setupStore(t)
	old := newOfflineRunner
	defer func() { newOfflineRunner = old }()
	newOfflineRunner = func(config.OfflineEvalConfig) prompteval.OfflineRunner {
		return fakeOfflineRunner{err: os.ErrNotExist}
	}

	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{
		{ID: "unavailable-variant", Prompt: "some prompt", Provider: "claude", Model: "opus"},
	})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "c1", Input: "hi", Assertions: []prompteval.Assertion{{Type: "contains", Value: "x"}}},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for an UNAVAILABLE verdict under default fail-closed policy, got 0. output: %s", output)
	}
}

// TestCmdEvaluationOfflineRunFailsOnHighScoreRunnerFailure is the regression
// case for the reported bug: a runner (e.g. promptfoo) can report a high
// numeric Score while its own success/gradingResult.pass signal says the run
// failed. The verdict must be FAIL and enrollment must be denied — deriving
// the verdict from Score alone silently passed this case before the fix.
func TestCmdEvaluationOfflineRunFailsOnHighScoreRunnerFailure(t *testing.T) {
	dir := setupStore(t)
	old := newOfflineRunner
	defer func() { newOfflineRunner = old }()
	failed := false
	newOfflineRunner = func(config.OfflineEvalConfig) prompteval.OfflineRunner {
		return fakeOfflineRunner{score: 1, forcePassed: &failed}
	}

	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{
		{ID: "boundary-variant", Prompt: "SCREENED_PROMPT: avoid unsafe content", Provider: "openai", Model: "gpt-test"},
	})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "case-one", Input: "user asks question", Assertions: []prompteval.Assertion{{Type: "not-contains", Value: "unsafe"}}},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit when the runner reports failure despite score=1, got 0. output: %s", output)
	}

	digest := prompteval.Digest([]byte("SCREENED_PROMPT: avoid unsafe content"))
	gateCode, gateOut := runCLI(t, "evaluation", "offline", "gate", "boundary-variant", digest, "--json")
	if gateCode == 0 {
		t.Fatalf("expected gate to deny enrollment for a runner-failed verdict, got allow. output: %s", gateOut)
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

// TestCmdEvaluationOfflineRunRejectsMixedProviderModel is the regression
// case for the reported bug: the scope requires running variants "against
// the same model/provider settings" so the batch is a fair comparison, but
// the CLI accepted a batch mixing providers/models without complaint.
func TestCmdEvaluationOfflineRunRejectsMixedProviderModel(t *testing.T) {
	dir := setupStore(t)
	old := newOfflineRunner
	defer func() { newOfflineRunner = old }()
	newOfflineRunner = func(config.OfflineEvalConfig) prompteval.OfflineRunner {
		return fakeOfflineRunner{score: 1}
	}

	variantsPath := filepath.Join(dir, "variants.json")
	goldenPath := filepath.Join(dir, "golden.json")
	writeJSONFile(t, variantsPath, []prompteval.CandidateVariant{
		{ID: "variant-a", Prompt: "PROMPT A", Provider: "openai", Model: "gpt-a"},
		{ID: "variant-b", Prompt: "PROMPT B", Provider: "anthropic", Model: "claude-b"},
	})
	writeJSONFile(t, goldenPath, []offlineGoldenCase{
		{CaseID: "c1", Input: "hi"},
	})

	code, output := runCLI(t, "evaluation", "offline", "run", "--variants", variantsPath, "--golden", goldenPath, "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for a batch mixing provider/model settings, got 0. output: %s", output)
	}
}

func TestCmdEvaluationOfflineGateDeniesMissingVerdict(t *testing.T) {
	setupStore(t)
	code, output := runCLI(t, "evaluation", "offline", "gate", "no-such-variant", "no-such-digest", "--json")
	if code == 0 {
		t.Fatalf("expected nonzero exit for a missing verdict (fail-closed default), got 0. output: %s", output)
	}
}
