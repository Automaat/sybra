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
	}
	root, err := findModuleRoot(wd)
	if err != nil {
		t.Fatalf("find module root: %v", err)
	}
	pt, err := loadPkgTypes(filepath.Join(root, "internal", "config"))
	if err != nil {
		t.Fatalf("load package types: %v", err)
	}

	var sections []section
	walk(pt, reflect.ValueOf(*config.DefaultConfig()), "Config", "", nil, map[string]bool{}, &sections)
	doc := render(sections)

	for _, want := range []string{
		"## ProviderEntryConfig (`providers.claude`)",
		"## ProviderEntryConfig (`providers.codex`)",
		"## ProviderEntryConfig (`providers.copilot`)",
		"| `providers.claude.enabled` |",
		"| `providers.codex.enabled` |",
		"| `providers.copilot.enabled` |",
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("generated docs missing %q", want)
		}
	}
}
