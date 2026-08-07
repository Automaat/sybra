package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func TestWalkRendersRepeatedLocalStructsAtEachYAMLPath(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
		panic("unreachable")
	}
	root, err := findModuleRoot(wd)
	if err != nil {
		t.Fatalf("find module root: %v", err)
		panic("unreachable")
	}
	pt, err := loadPkgTypes(filepath.Join(root, "internal", "config"))
	if err != nil {
		t.Fatalf("load package types: %v", err)
		panic("unreachable")
	}

	var sections []section
	walk(pt, reflect.ValueOf(*config.DefaultConfig()), "Config", "", nil, map[string]bool{}, &sections)
	doc := render(sections)

	for _, want := range []string{
		"### ProviderEntryConfig (`execution.providers.claude`)",
		"### ProviderEntryConfig (`execution.providers.codex`)",
		"### ProviderEntryConfig (`execution.providers.copilot`)",
		"| `execution.providers.claude.enabled` |",
		"| `execution.providers.codex.enabled` |",
		"| `execution.providers.copilot.enabled` |",
		"| `routing` | Adaptive provider-routing policy that tunes experiment weights from observed execution outcomes. | `routing` |",
		"## Routing",
		"### RoutingConfig (`routing`)",
		"### GitHubWebhookConfig (`integrations.github.webhook`)",
		"| `integrations.github.webhook.enabled` |",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("generated docs missing %q", want)
		}
	}
	if strings.Contains(doc, "### WebhookConfig (`webhook`)") {
		t.Fatal("generated docs retained the deprecated top-level WebhookConfig section")
	}
}
