package modeltier

import "testing"

func TestNormalizeAliasRejectsUnknownProvider(t *testing.T) {
	for _, alias := range []string{"", "cheap", "sonnet", "supercheap", "super_cheap", "haiku", "expensive", "opus"} {
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
	tests := []struct {
		provider string
		alias    string
		want     string
	}{
		{provider: "opencode", alias: "opus", want: "openrouter/z-ai/glm-5.2"},
		{provider: "codex", alias: "expensive", want: "gpt-5.6-sol"},
		{provider: "claude", alias: "cheap", want: "sonnet"},
		{provider: "claude", alias: "supercheap", want: "haiku"},
		{provider: "codex", alias: "super_cheap", want: "gpt-5.6-luna"},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"_"+tt.alias, func(t *testing.T) {
			got, ok := NormalizeAlias(tt.provider, tt.alias)
			if !ok {
				t.Fatalf("NormalizeAlias(%s, %s) ok=false, want true", tt.provider, tt.alias)
			}
			if got != tt.want {
				t.Fatalf("NormalizeAlias(%s, %s) = %q, want %q", tt.provider, tt.alias, got, tt.want)
			}
		})
	}
}

func TestInferTier(t *testing.T) {
	tests := []struct {
		model string
		want  Tier
		ok    bool
	}{
		{model: "", want: Cheap, ok: true},
		{model: "cheap", want: Cheap, ok: true},
		{model: "sonnet", want: Cheap, ok: true},
		{model: "supercheap", want: SuperCheap, ok: true},
		{model: "super_cheap", want: SuperCheap, ok: true},
		{model: "haiku", want: SuperCheap, ok: true},
		{model: "expensive", want: Expensive, ok: true},
		{model: "opus", want: Expensive, ok: true},
		{model: "gpt-5.6-sol", want: Expensive, ok: true},
		{model: "gpt-5.6-terra", want: Cheap, ok: true},
		{model: "gpt-5.6-luna", want: SuperCheap, ok: true},
		// The bare generation alias resolves to Sol, and must not be matched
		// by a Contains rule that would also swallow -terra and -luna.
		{model: "gpt-5.6", want: Expensive, ok: true},
		// Retired codex slugs stay classifiable for historical run records.
		{model: "gpt-5.5", want: Expensive, ok: true},
		{model: "gpt-5.4", want: Cheap, ok: true},
		{model: "gpt-5.4-mini", want: SuperCheap, ok: true},
		{model: "claude-sonnet-4.6", want: Cheap, ok: true},
		{model: "claude-haiku-4.5", want: SuperCheap, ok: true},
		{model: "custom-model", ok: false},
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
