package workflow

import (
	"embed"
	"fmt"
	"path/filepath"

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

// SyncBuiltins writes built-in workflows to the store directory if they
// don't already exist. Existing user-modified files are not overwritten.
func SyncBuiltins(store *Store) error {
	defs, err := BuiltinDefinitions()
	if err != nil {
		return err
	}
	for i := range defs {
		if _, getErr := store.Get(defs[i].ID); getErr == nil {
			continue // already exists
		}
		if sErr := store.Save(defs[i]); sErr != nil {
			return fmt.Errorf("sync builtin %s: %w", defs[i].ID, sErr)
		}
	}
	return nil
}
