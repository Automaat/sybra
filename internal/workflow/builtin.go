package workflow

import (
	"embed"
	"fmt"
	"path/filepath"
	"reflect"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// BuiltinDefinitions returns all embedded default workflow definitions.
func BuiltinDefinitions() ([]Definition, error) {
	entries, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("read builtin dir: %w", err)
	}

	var defs []Definition
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		data, rErr := builtinFS.ReadFile("builtin/" + e.Name())
		if rErr != nil {
			return nil, fmt.Errorf("read builtin %s: %w", e.Name(), rErr)
		}
		var def Definition
		if uErr := yaml.Unmarshal(data, &def); uErr != nil {
			return nil, fmt.Errorf("parse builtin %s: %w", e.Name(), uErr)
		}
		def.Builtin = true
		defs = append(defs, def)
	}
	return defs, nil
}

// SyncBuiltins writes built-in workflows to the store directory. For each
// embedded definition:
//
//   - If no stored version exists, it is saved.
//   - If a stored version exists with Builtin=true and its semantic content
//     differs from the embedded version, it is overwritten. This repairs
//     drift from older app versions that seeded now-broken definitions.
//   - If a stored version exists with Builtin=false (user cleared the flag
//     to opt out of sync), it is preserved.
func SyncBuiltins(store *Store) error {
	defs, err := BuiltinDefinitions()
	if err != nil {
		return err
	}
	for i := range defs {
		existing, getErr := store.Get(defs[i].ID)
		if getErr != nil {
			// Not present yet → create.
			if sErr := store.Save(defs[i]); sErr != nil {
				return fmt.Errorf("sync builtin %s: %w", defs[i].ID, sErr)
			}
			continue
		}
		if !existing.Builtin {
			continue // user opted out by clearing the builtin flag
		}
		if builtinsEqual(existing, defs[i]) {
			continue
		}
		// Preserve creation time; Save() refreshes UpdatedAt.
		defs[i].CreatedAt = existing.CreatedAt
		if sErr := store.Save(defs[i]); sErr != nil {
			return fmt.Errorf("sync builtin %s: %w", defs[i].ID, sErr)
		}
	}
	return nil
}

// builtinsEqual compares two definitions ignoring timestamps. Timestamps are
// set by Save() each write, so byte-level comparison would always diverge.
func builtinsEqual(a, b Definition) bool {
	a.CreatedAt = b.CreatedAt
	a.UpdatedAt = b.UpdatedAt
	return reflect.DeepEqual(a, b)
}
