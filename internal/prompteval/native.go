package prompteval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/llmexec"
)

// NativeRunner evaluates a Spec with a single-shot prompt via
// internal/llmexec and deterministic assertions, scored locally. It never
// requires Node/promptfoo, so the gate still works on the mise-only server.
//
// llmexec.Result exposes no cost or latency, so CostUSD always reports 0 here
// (a genuine measurement gap, not a bug) while LatencyMS is measured locally
// around the call. Do not edit internal/llmexec to add cost/latency for this
// package — it is a shared one-shot CLI runner used by several callers.
type NativeRunner struct{}

// NewNativeRunner constructs a NativeRunner.
func NewNativeRunner() *NativeRunner { return &NativeRunner{} }

// Name implements OfflineRunner.
func (r *NativeRunner) Name() string { return "native" }

// Available implements OfflineRunner. The native runner has no external
// binary dependency beyond the provider CLIs llmexec already probes.
func (r *NativeRunner) Available() bool { return true }

// Run implements OfflineRunner.
func (r *NativeRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	start := time.Now()
	out, err := llmexec.RunJSON(ctx, spec.Input, llmexec.Options{
		Provider: spec.Variant.Provider,
		Models:   map[string]string{spec.Variant.Provider: spec.Variant.Model},
	})
	latencyMS := time.Since(start).Milliseconds()
	if err != nil {
		return Result{}, fmt.Errorf("native runner: %w", err)
	}

	results := make([]AssertionResult, 0, len(spec.Assertions))
	var deterministic, passed int
	for _, a := range spec.Assertions {
		ar := evaluateAssertion(a, out.Text, latencyMS)
		results = append(results, ar)
		if a.Type == "model-graded" {
			continue
		}
		deterministic++
		if ar.Passed {
			passed++
		}
	}

	score := 1.0
	if deterministic > 0 {
		score = float64(passed) / float64(deterministic)
	}

	return Result{
		Output:     out.Text,
		Assertions: results,
		Score:      score,
		CostUSD:    0,
		LatencyMS:  latencyMS,
	}, nil
}

func evaluateAssertion(a Assertion, output string, latencyMS int64) AssertionResult {
	switch a.Type {
	case "contains":
		ok := strings.Contains(output, a.Value)
		return AssertionResult{Type: a.Type, Passed: ok, Detail: matchDetail(ok, a.Value)}
	case "not-contains":
		ok := !strings.Contains(output, a.Value)
		return AssertionResult{Type: a.Type, Passed: ok, Detail: matchDetail(ok, a.Value)}
	case "regex":
		re, err := regexp.Compile(a.Value)
		if err != nil {
			return AssertionResult{Type: a.Type, Passed: false, Detail: fmt.Sprintf("invalid regex %q: %v", a.Value, err)}
		}
		ok := re.MatchString(output)
		return AssertionResult{Type: a.Type, Passed: ok, Detail: matchDetail(ok, a.Value)}
	case "is-json":
		var v any
		ok := json.Unmarshal([]byte(output), &v) == nil
		return AssertionResult{Type: a.Type, Passed: ok, Detail: matchDetail(ok, "valid JSON")}
	case "latency":
		maxDur, err := time.ParseDuration(a.Value)
		if err != nil {
			return AssertionResult{Type: a.Type, Passed: false, Detail: fmt.Sprintf("invalid duration %q: %v", a.Value, err)}
		}
		ok := time.Duration(latencyMS)*time.Millisecond <= maxDur
		return AssertionResult{Type: a.Type, Passed: ok, Detail: fmt.Sprintf("latency %dms vs max %s", latencyMS, a.Value)}
	case "model-graded":
		return AssertionResult{Type: a.Type, Passed: true, Detail: "model-graded assertions are advisory only and never fail a run"}
	default:
		return AssertionResult{Type: a.Type, Passed: false, Detail: fmt.Sprintf("unknown assertion type %q", a.Type)}
	}
}

func matchDetail(ok bool, want string) string {
	if ok {
		return fmt.Sprintf("matched %q", want)
	}
	return fmt.Sprintf("did not match %q", want)
}
