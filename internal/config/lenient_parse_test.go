package config

import (
	"errors"
	"strings"
	"testing"
)

// TestParseFileConfigLenient covers the split the CLI depends on: a schema
// error must come back beside a usable config, never instead of it, and it
// must carry ErrUnknownConfigKey so a caller can tell "this key is stale" from
// "this file is unreadable". The two rejection passes disagree about which
// keys they own — a namespace key is refused by the document normalizer and
// everything else by the schema walk — so both are covered here.
func TestParseFileConfigLenient(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		yaml string
		key  string
	}{
		{"top level", "schema_version: 2\nfuture_namespace:\n  enabled: true\n", "future_namespace"},
		{"execution namespace", "schema_version: 2\nexecution:\n  bogus:\n    enabled: true\n", "execution.bogus"},
		{"integrations namespace", "schema_version: 2\nintegrations:\n  slack:\n    enabled: true\n", "integrations.slack"},
		{"storage paths namespace", "schema_version: 2\nstorage:\n  paths:\n    bogus: /tmp/x\n", "storage.paths.bogus"},
		{"instance namespace", "schema_version: 2\ninstance:\n  bogus: true\n", "instance.bogus"},
		{"nested field", "schema_version: 2\nexecution:\n  agent:\n    bogus_field: 1\n", "agent.bogus_field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, schemaErr, err := parseFileConfigLenient([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("err = %v, want nil (a stale key is not unparseable input)", err)
			}
			if cfg == nil {
				t.Fatal("config = nil; a lenient parse must still return the document")
			}
			if schemaErr == nil {
				t.Fatalf("schemaErr = nil for %q", tt.key)
			}
			if !errors.Is(schemaErr, ErrUnknownConfigKey) {
				t.Errorf("schemaErr = %v, does not match ErrUnknownConfigKey", schemaErr)
			}
			if !strings.Contains(schemaErr.Error(), tt.key) {
				t.Errorf("schemaErr = %q, want it to name %q", schemaErr, tt.key)
			}

			// The strict wrapper must still refuse the same input.
			if _, strictErr := ParseFileConfig([]byte(tt.yaml)); strictErr == nil {
				t.Error("ParseFileConfig accepted a config with an unknown key")
			}
		})
	}
}

// TestParseFileConfigLenient_ReportsEveryUnknownKey pins the one-pass
// contract, so an operator clearing template drift is not handed one key per
// run.
func TestParseFileConfigLenient_ReportsEveryUnknownKey(t *testing.T) {
	t.Parallel()
	const doc = "schema_version: 2\nintegrations:\n  slack:\n    enabled: true\nstorage:\n  bogus: true\nfuture_namespace:\n  enabled: true\n"

	_, schemaErr, err := parseFileConfigLenient([]byte(doc))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if schemaErr == nil {
		t.Fatal("schemaErr = nil")
	}
	for _, key := range []string{"integrations.slack", "storage.bogus", "future_namespace"} {
		if !strings.Contains(schemaErr.Error(), key) {
			t.Errorf("schemaErr = %q, want it to name %q", schemaErr, key)
		}
	}
}

// TestParseFileConfigLenient_CleanConfig proves the lenient path is not simply
// swallowing everything: a valid document reports no schema error, and an
// unparseable one is still a hard failure rather than a tolerated key.
func TestParseFileConfigLenient_CleanConfig(t *testing.T) {
	t.Parallel()

	cfg, schemaErr, err := parseFileConfigLenient([]byte("schema_version: 2\nexecution:\n  agent:\n    provider: claude\n"))
	if err != nil || schemaErr != nil {
		t.Fatalf("clean config: err=%v schemaErr=%v, want both nil", err, schemaErr)
	}
	if cfg == nil {
		t.Fatal("config = nil for a clean document")
	}

	if _, _, err := parseFileConfigLenient([]byte("::not yaml at all\n")); err == nil {
		t.Error("unparseable input produced no error")
	}
}

// TestNormalizeV2DocumentStaysStrict guards the exported wrapper: only the
// unexported lenient path may skip a key, since every migration and save path
// goes through this one.
func TestNormalizeV2DocumentStaysStrict(t *testing.T) {
	t.Parallel()
	cfg, _, err := parseFileConfigLenient([]byte("schema_version: 2\nintegrations:\n  slack:\n    enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("config = nil")
	}
	if _, _, err := NormalizeV2Document(cfg.root); err == nil {
		t.Fatal("NormalizeV2Document accepted an unknown namespace key")
	} else if !errors.Is(err, ErrUnknownConfigKey) {
		t.Errorf("err = %v, does not match ErrUnknownConfigKey", err)
	}
}
