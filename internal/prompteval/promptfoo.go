package prompteval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultPromptfooTimeout bounds a single `promptfoo eval` shell-out so a
// hung provider call can't wedge the offline eval CLI forever.
const defaultPromptfooTimeout = 5 * time.Minute

// PromptfooRunner shells out to the promptfoo CLI. Config generation,
// process execution, and output parsing are encapsulated in this file only —
// no other file in this package shells out or knows the promptfoo config
// shape.
type PromptfooRunner struct {
	// BinaryPath overrides the promptfoo binary resolved from PATH; empty
	// means "promptfoo".
	BinaryPath string
}

// NewPromptfooRunner constructs a PromptfooRunner. binaryPath may be empty.
func NewPromptfooRunner(binaryPath string) *PromptfooRunner {
	return &PromptfooRunner{BinaryPath: binaryPath}
}

// Name implements OfflineRunner.
func (r *PromptfooRunner) Name() string { return "promptfoo" }

// Available implements OfflineRunner by probing the binary on PATH.
func (r *PromptfooRunner) Available() bool {
	_, err := exec.LookPath(r.binary())
	return err == nil
}

func (r *PromptfooRunner) binary() string {
	if r.BinaryPath != "" {
		return r.BinaryPath
	}
	return "promptfoo"
}

// Run implements OfflineRunner: generates a promptfooconfig.yaml from spec,
// runs `promptfoo eval -o out.json` with a timeout, and parses the result.
func (r *PromptfooRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultPromptfooTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "prompteval-promptfoo-*")
	if err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: mkdir temp: %w", err)
	}
	defer os.RemoveAll(dir)

	configPath := filepath.Join(dir, "promptfooconfig.yaml")
	outPath := filepath.Join(dir, "out.json")
	logPath := filepath.Join(dir, "run.log")

	configData, err := generateConfig(spec)
	if err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: generate config: %w", err)
	}
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: write config: %w", err)
	}

	logFile, err := os.Create(logPath)
	if err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: create log: %w", err)
	}
	defer logFile.Close()

	// #nosec G204 -- binary path and args are built from the runner's own
	// config and a fixed argument list, never from unsanitized user input.
	cmd := exec.CommandContext(runCtx, r.binary(), "eval", "-c", configPath, "-o", outPath, "--no-cache")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if runErr := cmd.Run(); runErr != nil {
		return Result{}, fmt.Errorf("promptfoo runner: eval: %w (see %s)", runErr, logPath)
	}

	outData, err := os.ReadFile(outPath)
	if err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: read output: %w", err)
	}
	return parseOutput(outData)
}

// promptfooConfig is the minimal subset of promptfooconfig.yaml this package
// generates: one prompt, one provider, one test with its assertions.
type promptfooConfig struct {
	Prompts   []string             `yaml:"prompts"`
	Providers []promptfooProvider  `yaml:"providers"`
	Tests     []promptfooTestEntry `yaml:"tests"`
}

type promptfooProvider struct {
	ID string `yaml:"id"`
}

type promptfooTestEntry struct {
	Vars   map[string]string `yaml:"vars"`
	Assert []promptfooAssert `yaml:"assert"`
}

type promptfooAssert struct {
	Type  string `yaml:"type"`
	Value string `yaml:"value,omitempty"`
}

// generateConfig marshals spec into promptfooconfig.yaml via yaml.Marshal so
// prompt/assertion text is always properly quoted — never string
// concatenation, which would let injected `"` or newlines smuggle extra YAML
// keys into the generated config.
//
// The prompt template composes the variant's own resolved prompt/skill body
// ({{variantPrompt}}) ahead of the golden case input ({{input}}) so the
// bytes promptfoo actually sends to the provider are the digested candidate
// prompt, not just the fixture input. Both are passed as vars (never
// interpolated into the template string itself) so injected `{{`/YAML
// syntax in either one can't reshape the generated config or template.
func generateConfig(spec Spec) ([]byte, error) {
	asserts := make([]promptfooAssert, 0, len(spec.Assertions))
	for _, a := range spec.Assertions {
		asserts = append(asserts, promptfooAssert(a))
	}
	cfg := promptfooConfig{
		Prompts:   []string{"{{variantPrompt}}\n\n{{input}}"},
		Providers: []promptfooProvider{{ID: fmt.Sprintf("%s:%s", spec.Variant.Provider, spec.Variant.Model)}},
		Tests: []promptfooTestEntry{
			{
				Vars:   map[string]string{"input": spec.Input, "variantPrompt": spec.Variant.Prompt},
				Assert: asserts,
			},
		},
	}
	return yaml.Marshal(cfg)
}

// promptfooOutput is the subset of `promptfoo eval -o out.json` this package
// reads. Real promptfoo output carries far more fields; anything not listed
// here is ignored.
type promptfooOutput struct {
	Results struct {
		Results []promptfooResult `json:"results"`
	} `json:"results"`
}

type promptfooResult struct {
	Success   bool    `json:"success"`
	Score     float64 `json:"score"`
	LatencyMS int64   `json:"latencyMs"`
	Cost      float64 `json:"cost"`
	Response  struct {
		Output string `json:"output"`
	} `json:"response"`
	GradingResult struct {
		Pass             bool `json:"pass"`
		ComponentResults []struct {
			Assertion struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"assertion"`
			Pass   bool   `json:"pass"`
			Reason string `json:"reason"`
		} `json:"componentResults"`
	} `json:"gradingResult"`
}

// parseOutput normalizes promptfoo's out.json into a Result. Truncated,
// malformed, or empty output returns an error — never a zero-value Result —
// so the caller maps it to Status unavailable instead of a silent pass.
func parseOutput(data []byte) (Result, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Result{}, fmt.Errorf("promptfoo runner: empty output")
	}
	var out promptfooOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return Result{}, fmt.Errorf("promptfoo runner: parse output: %w", err)
	}
	if len(out.Results.Results) == 0 {
		return Result{}, fmt.Errorf("promptfoo runner: no results in output")
	}
	res := out.Results.Results[0]

	assertions := make([]AssertionResult, 0, len(res.GradingResult.ComponentResults))
	for _, cr := range res.GradingResult.ComponentResults {
		assertions = append(assertions, AssertionResult{
			Type:   cr.Assertion.Type,
			Passed: cr.Pass,
			Detail: cr.Reason,
		})
	}

	return Result{
		Output:     res.Response.Output,
		Assertions: assertions,
		Score:      res.Score,
		CostUSD:    res.Cost,
		LatencyMS:  res.LatencyMS,
	}, nil
}
