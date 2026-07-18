package llmjob

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/provider"
)

type testOut struct {
	OK bool `json:"ok"`
}

func TestRun(t *testing.T) {
	tests := []struct {
		name        string
		results     []llmexec.Result
		validate    func(*testOut) error
		maxRepairs  int
		wantRepairs int
		wantErr     string
	}{
		{
			name:        "decode-success",
			results:     []llmexec.Result{{Provider: "claude", Text: `{"ok":true}`}},
			wantRepairs: 0,
		},
		{
			name:        "decode-fenced-json-without-repair",
			results:     []llmexec.Result{{Provider: "claude", Text: "Here is the result:\n```json\n{\"ok\":true}\n```"}},
			wantRepairs: 0,
		},
		{
			name:        "repair-on-bad-json",
			results:     []llmexec.Result{{Provider: "claude", Text: `{`}, {Provider: "claude", Text: `{"ok":true}`}},
			wantRepairs: 1,
		},
		{
			name:    "repair-on-validate-fail",
			results: []llmexec.Result{{Provider: "claude", Text: `{"ok":false}`}, {Provider: "claude", Text: `{"ok":true}`}},
			validate: func(out *testOut) error {
				if !out.OK {
					return errors.New("not ok")
				}
				return nil
			},
			wantRepairs: 1,
		},
		{
			name:       "exhausted-repairs",
			results:    []llmexec.Result{{Provider: "claude", Text: `{`}, {Provider: "claude", Text: `{`}},
			maxRepairs: 1,
			wantErr:    "job failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prompts []string
			restore := stubRunner(func(_ context.Context, prompt string, _ llmexec.Options) (llmexec.Result, error) {
				prompts = append(prompts, prompt)
				if len(prompts) > len(tt.results) {
					t.Fatalf("unexpected attempt %d", len(prompts))
				}
				return tt.results[len(prompts)-1], nil
			})
			defer restore()

			out, meta, err := Run(context.Background(), "prompt", Spec[testOut]{
				Name:       "test",
				Tier:       Cheap,
				Validate:   tt.validate,
				MaxRepairs: tt.maxRepairs,
			}, llmexec.Options{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !out.OK {
				t.Fatalf("out.OK = false")
			}
			if meta.Repairs != tt.wantRepairs {
				t.Fatalf("repairs = %d, want %d", meta.Repairs, tt.wantRepairs)
			}
			if tt.wantRepairs > 0 && !strings.Contains(prompts[len(prompts)-1], "previous output was invalid") {
				t.Fatalf("repair prompt missing invalid-output context: %q", prompts[len(prompts)-1])
			}
		})
	}
}

// TestRunAttemptTimeoutGivesEachAttemptFreshBudget covers the umbrella
// planner regression (#1555): a single shared deadline let attempt 1 consume
// the whole budget on a hung provider call, so attempts 2/3 died instantly on
// an already-expired context. With AttemptTimeout set, each attempt gets its
// own context.WithTimeout derived fresh from ctx, so a provider that sleeps
// past its own per-attempt deadline still leaves later attempts a full
// budget to actually run in.
func TestRunAttemptTimeoutGivesEachAttemptFreshBudget(t *testing.T) {
	const attemptTimeout = 20 * time.Millisecond
	calls := 0
	restore := stubRunner(func(ctx context.Context, _ string, _ llmexec.Options) (llmexec.Result, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return llmexec.Result{Provider: "claude"}, ctx.Err()
		}
		return llmexec.Result{Provider: "claude", Text: `{"ok":true}`}, nil
	})
	defer restore()

	_, meta, err := Run(context.Background(), "prompt", Spec[testOut]{
		Name:           "umbrella-order",
		Tier:           Cheap,
		AttemptTimeout: attemptTimeout,
	}, llmexec.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (attempt 2 must run after attempt 1's deadline expires)", calls)
	}
	if meta.Provider != "claude" {
		t.Fatalf("meta.Provider = %q, want %q", meta.Provider, "claude")
	}
}

// TestRunAttemptTimeoutFinalErrorNamesProvider covers the other half of
// #1555: when every attempt fails, the wrapped error must still name the
// provider and model that failed rather than reporting an empty provider
// (previously lost because RunJSON's error-path Results were zero-valued).
// Tier: Standard matches the umbrella planner's actual Spec (llmjob.Standard
// maps claude to sonnet), so this doubles as a regression test for the
// missing-model-name defect found during manual testing.
func TestRunAttemptTimeoutFinalErrorNamesProvider(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, _ llmexec.Options) (llmexec.Result, error) {
		return llmexec.Result{Provider: "claude"}, errors.New("signal: killed")
	})
	defer restore()

	_, meta, err := Run(context.Background(), "prompt", Spec[testOut]{
		Name:           "umbrella-order",
		Tier:           Standard,
		AttemptTimeout: 20 * time.Millisecond,
	}, llmexec.Options{})
	if err == nil {
		t.Fatal("Run: want error")
	}
	if !strings.Contains(err.Error(), `provider "claude"`) {
		t.Fatalf("err = %v, want it to name provider %q", err, "claude")
	}
	if !strings.Contains(err.Error(), `model "sonnet"`) {
		t.Fatalf("err = %v, want it to name model %q", err, "sonnet")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v, want it to report all 3 attempts made", err)
	}
	if meta.Provider != "claude" {
		t.Fatalf("meta.Provider = %q, want %q", meta.Provider, "claude")
	}
}

func TestRunAvoidProviderExcludedFromOrder(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, opts llmexec.Options) (llmexec.Result, error) {
		if opts.Provider == "claude" {
			t.Fatalf("Provider = claude, want avoided provider excluded from preferred provider")
		}
		if opts.Gate == nil || opts.Gate.IsHealthy("claude") {
			t.Fatalf("avoided provider was not marked unhealthy")
		}
		return llmexec.Result{Provider: opts.Provider, Text: `{"ok":true}`}, nil
	})
	defer restore()

	_, meta, err := Run(context.Background(), "prompt", Spec[testOut]{
		Name:          "avoid",
		Tier:          Cheap,
		AvoidProvider: "claude",
	}, llmexec.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if meta.Provider == "claude" {
		t.Fatalf("provider = claude, want different provider")
	}
}

func TestRunSetsTierModels(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, opts llmexec.Options) (llmexec.Result, error) {
		want := map[string]string{"claude": "sonnet", "codex": "gpt-5.4", "copilot": "claude-sonnet-4.6", "opencode": "openrouter/deepseek/deepseek-v4-flash"}
		if !reflect.DeepEqual(opts.Models, want) {
			t.Fatalf("models = %#v, want %#v", opts.Models, want)
		}
		return llmexec.Result{Provider: "claude", Text: `{"ok":true}`}, nil
	})
	defer restore()

	if _, _, err := Run(context.Background(), "prompt", Spec[testOut]{Name: "models", Tier: Cheap}, llmexec.Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunSetsSuperCheapTierModels(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, opts llmexec.Options) (llmexec.Result, error) {
		want := map[string]string{"claude": "haiku", "codex": "gpt-5.4-mini", "copilot": "gpt-5-mini", "opencode": "openrouter/qwen/qwen3-32b"}
		if !reflect.DeepEqual(opts.Models, want) {
			t.Fatalf("models = %#v, want %#v", opts.Models, want)
		}
		return llmexec.Result{Provider: "claude", Text: `{"ok":true}`}, nil
	})
	defer restore()

	if _, _, err := Run(context.Background(), "prompt", Spec[testOut]{Name: "models", Tier: SuperCheap}, llmexec.Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestStandardTierIsCheapAlias(t *testing.T) {
	if Standard != Cheap {
		t.Fatalf("Standard = %d, want Cheap alias %d", Standard, Cheap)
	}
	if !reflect.DeepEqual(modelsFor(Standard), modelsFor(Cheap)) {
		t.Fatalf("Standard models differ from Cheap models")
	}
}

func TestRunPreservesExplicitModels(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, opts llmexec.Options) (llmexec.Result, error) {
		want := map[string]string{"claude": "opus", "codex": "gpt-5.4", "copilot": "claude-sonnet-4.6", "opencode": "openrouter/deepseek/deepseek-v4-flash"}
		if !reflect.DeepEqual(opts.Models, want) {
			t.Fatalf("models = %#v, want %#v", opts.Models, want)
		}
		return llmexec.Result{Provider: "claude", Text: `{"ok":true}`}, nil
	})
	defer restore()

	if _, _, err := Run(context.Background(), "prompt", Spec[testOut]{Name: "models", Tier: Cheap}, llmexec.Options{
		Models: map[string]string{"claude": "opus"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunPassesSchemaViaOptionsNotPrompt(t *testing.T) {
	const schema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	restore := stubRunner(func(_ context.Context, prompt string, opts llmexec.Options) (llmexec.Result, error) {
		if opts.Schema != schema {
			t.Fatalf("opts.Schema = %q, want %q", opts.Schema, schema)
		}
		if strings.Contains(prompt, schema) || strings.Contains(prompt, "Output schema:") {
			t.Fatalf("prompt must not embed the schema (double delivery risk): %q", prompt)
		}
		return llmexec.Result{Provider: "codex", Text: `{"ok":true}`}, nil
	})
	defer restore()

	if _, _, err := Run(context.Background(), "prompt", Spec[testOut]{
		Name:   "schema-passthrough",
		Tier:   Cheap,
		Schema: schema,
	}, llmexec.Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestModelsForClones(t *testing.T) {
	got := modelsFor(Standard)
	got["claude"] = "mutated"
	again := modelsFor(Standard)
	if again["claude"] != "sonnet" || again["codex"] != "gpt-5.4" || again["copilot"] != "claude-sonnet-4.6" || again["opencode"] != "openrouter/deepseek/deepseek-v4-flash" {
		t.Fatalf("modelsFor did not clone standard row: %#v", again)
	}
}

func stubRunner(fn runnerFunc) func() {
	prev := runJSON
	runJSON = fn
	return func() {
		runJSON = prev
	}
}

// TestAvoidingGate_TypedNilBaseIsAbsentNotFatal covers the wrapper's own nil
// guard, which cannot catch the case that actually reaches it.
//
// `g.base == nil` is false when base holds a nil *provider.Checker — the shape
// the disabled-health-check config hands every gate consumer — so the wrapper
// delegates straight through, and only the base's nil-receiver contract keeps
// that from panicking (see provider.Checker's HealthGate section).
func TestAvoidingGate_TypedNilBaseIsAbsentNotFatal(t *testing.T) {
	t.Parallel()

	g := avoidingGate{base: (*provider.Checker)(nil), avoid: "codex"}

	if !g.IsHealthy("claude") {
		t.Error("IsHealthy(claude) = false; an absent base gate blocks nothing")
	}
	if g.IsHealthy("codex") {
		t.Error("IsHealthy(codex) = true; the avoided provider must still be refused")
	}
	if got := g.Reason("claude"); got != "" {
		t.Errorf("Reason(claude) = %q, want empty", got)
	}
	if got := g.Reason("codex"); got != "avoided for independence" {
		t.Errorf("Reason(codex) = %q, want the avoid reason", got)
	}
}
