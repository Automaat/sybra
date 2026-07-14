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
