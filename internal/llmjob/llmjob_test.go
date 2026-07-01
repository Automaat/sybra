package llmjob

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/llmexec"
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
		want := map[string]string{"claude": "haiku", "codex": "gpt-5.4-mini", "copilot": "gpt-5-mini"}
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

func TestRunPreservesExplicitModels(t *testing.T) {
	restore := stubRunner(func(_ context.Context, _ string, opts llmexec.Options) (llmexec.Result, error) {
		want := map[string]string{"claude": "opus", "codex": "gpt-5.4-mini", "copilot": "gpt-5-mini"}
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

func TestModelsForClones(t *testing.T) {
	got := modelsFor(Standard)
	got["claude"] = "mutated"
	again := modelsFor(Standard)
	if again["claude"] != "sonnet" || again["codex"] != "gpt-5.5" || again["copilot"] != "" {
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
