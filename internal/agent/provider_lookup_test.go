package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderLookupDefaultsOnlyEmptyName(t *testing.T) {
	t.Parallel()

	prov, err := lookupProvider("")
	if err != nil {
		t.Fatalf("lookupProvider(empty): %v", err)
	}
	if prov.Name() != "claude" {
		t.Fatalf("lookupProvider(empty) = %q, want claude", prov.Name())
	}

	if _, err := lookupProvider("future-provider"); err == nil || !strings.Contains(err.Error(), "unknown agent provider") {
		t.Fatalf("lookupProvider(unknown) err = %v, want unknown provider error", err)
	}
}

func TestUnknownProviderRejectedByInvocationReplayAndConvoParsing(t *testing.T) {
	t.Parallel()

	if name, _, _, _, err := buildHeadlessInvocation(&Agent{Provider: ""}, RunConfig{Prompt: "ok"}); err != nil || name != "claude" {
		t.Fatalf("buildHeadlessInvocation(empty provider) = name %q err %v, want claude nil", name, err)
	}
	if _, _, _, _, err := buildHeadlessInvocation(&Agent{Provider: "future-provider"}, RunConfig{Prompt: "ok"}); err == nil {
		t.Fatal("buildHeadlessInvocation(unknown provider) succeeded, want error")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "agent.ndjson")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ParseLogFile(path, 0, "")
	if err != nil {
		t.Fatalf("ParseLogFile(empty provider): %v", err)
	}
	if len(events) != 1 || events[0].Content != "hello" {
		t.Fatalf("ParseLogFile(empty provider) = %+v, want assistant hello", events)
	}
	if _, err := ParseLogFile(path, 0, "future-provider"); err == nil {
		t.Fatal("ParseLogFile(unknown provider) succeeded, want error")
	}

	if _, err := parseConvoEvent("future-provider", []byte(line)); err == nil || !strings.Contains(err.Error(), "unknown agent provider") {
		t.Fatalf("parseConvoEvent(unknown provider) err = %v, want unknown provider error", err)
	}
}
