package modeltier

import "testing"

func TestNormalizeAliasRejectsUnknownProvider(t *testing.T) {
	for _, alias := range []string{"", "sonnet", "haiku", "opus"} {
		got, ok := NormalizeAlias("unknown", alias)
		if ok {
			t.Fatalf("NormalizeAlias(unknown, %q) ok=true with model %q, want ok=false", alias, got)
		}
		if got != alias {
			t.Fatalf("NormalizeAlias(unknown, %q) = %q, want original alias", alias, got)
		}
	}
}

func TestNormalizeAliasMapsKnownProvider(t *testing.T) {
	got, ok := NormalizeAlias("opencode", "opus")
	if !ok {
		t.Fatal("NormalizeAlias(opencode, opus) ok=false, want true")
	}
	if got != "openrouter/z-ai/glm-5.2" {
		t.Fatalf("NormalizeAlias(opencode, opus) = %q", got)
	}
}

func TestInferTier(t *testing.T) {
	tests := []struct {
		model string
		want  Tier
		ok    bool
	}{
		{model: "", want: Cheap, ok: true},
		{model: "sonnet", want: Cheap, ok: true},
		{model: "haiku", want: SuperCheap, ok: true},
		{model: "opus", want: Expensive, ok: true},
		{model: "gpt-5.5", want: Expensive, ok: true},
		{model: "claude-sonnet-4.6", want: Cheap, ok: true},
		{model: "claude-sonnet-4-6", want: Cheap, ok: true},
		{model: "claude-haiku-4.5", want: SuperCheap, ok: true},
		{model: "claude-haiku-4-5-20251001", want: SuperCheap, ok: true},
		{model: "claude-opus-4-8", want: Expensive, ok: true},
		{model: "gemini-3.1-pro", want: Expensive, ok: true},
		{model: "custom-model", ok: false},
		{model: "my-sonnet-experiment", ok: false},
		{model: "foo-opus-bar", ok: false},
		{model: "experimental-gpt-5.4-turbo", ok: false},
		{model: "claude-sonnet-custom", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got, ok := InferTier(tt.model)
			if ok != tt.ok {
				t.Fatalf("InferTier(%q) ok=%v, want %v (tier=%q)", tt.model, ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Fatalf("InferTier(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
