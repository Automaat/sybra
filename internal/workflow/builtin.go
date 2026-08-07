package workflow

import (
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
		fileDefs, pErr := parseBuiltinFile(data)
		if pErr != nil {
			return nil, fmt.Errorf("parse builtin %s: %w", e.Name(), pErr)
		}
		for i := range fileDefs {
			fileDefs[i].Builtin = true
		}
		defs = append(defs, fileDefs...)
	}
	return defs, nil
}

type builtinFileMeta struct {
	Kind string `yaml:"kind"`
}

type handoffBuiltinTemplate struct {
	Kind     string                  `yaml:"kind"`
	Trigger  Trigger                 `yaml:"trigger"`
	Variants []handoffBuiltinVariant `yaml:"variants"`
}

type handoffBuiltinVariant struct {
	ID          string                    `yaml:"id"`
	Name        string                    `yaml:"name"`
	Description string                    `yaml:"description"`
	Tag         string                    `yaml:"tag"`
	ExcludeTags []string                  `yaml:"exclude_tags"`
	Step        handoffBuiltinVariantStep `yaml:"step"`
}

type handoffBuiltinVariantStep struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Status string `yaml:"status"`
}

func parseBuiltinFile(data []byte) ([]Definition, error) {
	var meta builtinFileMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.Kind == "handoff_template" {
		return expandHandoffBuiltinTemplate(data)
	}

	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, err
	}
	return []Definition{def}, nil
}

func expandHandoffBuiltinTemplate(data []byte) ([]Definition, error) {
	var tmpl handoffBuiltinTemplate
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		return nil, err
	}
	if strings.TrimSpace(tmpl.Trigger.On) == "" {
		return nil, errors.New("handoff template missing trigger.on")
	}
	if len(tmpl.Variants) == 0 {
		return nil, errors.New("handoff template has no variants")
	}

	defs := make([]Definition, 0, len(tmpl.Variants))
	for i := range tmpl.Variants {
		variant := &tmpl.Variants[i]
		if err := validateHandoffBuiltinVariant(*variant); err != nil {
			return nil, err
		}
		trigger := tmpl.Trigger
		trigger.Conditions = make([]Condition, 0, len(variant.ExcludeTags)+1)
		trigger.Conditions = append(trigger.Conditions, Condition{
			Field:    "task.tags",
			Operator: "contains",
			Value:    variant.Tag,
		})
		for _, tag := range variant.ExcludeTags {
			trigger.Conditions = append(trigger.Conditions, Condition{
				Field:    "task.tags",
				Operator: "not_contains",
				Value:    tag,
			})
		}

		defs = append(defs, Definition{
			ID:          variant.ID,
			Name:        variant.Name,
			Description: variant.Description,
			Trigger:     trigger,
			Steps: []Step{{
				ID:   variant.Step.ID,
				Name: variant.Step.Name,
				Type: StepSetStatus,
				Config: StepConfig{
					Status: variant.Step.Status,
				},
				Next: []Transition{{GoTo: ""}},
			}},
		})
	}
	return defs, nil
}

func validateHandoffBuiltinVariant(variant handoffBuiltinVariant) error {
	switch {
	case strings.TrimSpace(variant.ID) == "":
		return errors.New("handoff template variant missing id")
	case strings.TrimSpace(variant.Name) == "":
		return fmt.Errorf("handoff template %s missing name", variant.ID)
	case strings.TrimSpace(variant.Tag) == "":
		return fmt.Errorf("handoff template %s missing tag", variant.ID)
	case strings.TrimSpace(variant.Step.ID) == "":
		return fmt.Errorf("handoff template %s missing step.id", variant.ID)
	case strings.TrimSpace(variant.Step.Name) == "":
		return fmt.Errorf("handoff template %s missing step.name", variant.ID)
	case strings.TrimSpace(variant.Step.Status) == "":
		return fmt.Errorf("handoff template %s missing step.status", variant.ID)
	default:
		return nil
	}
}

// SyncBuiltins writes built-in workflows to the store directory. For each
// embedded definition:
//
//   - If no stored version exists, it is saved.
//   - If a stored version exists with Builtin=true and its semantic content
//     differs from the embedded version, it is overwritten. This repairs
//     drift from older app versions that seeded now-broken definitions.
//   - If a stored version is Builtin=true but no longer exists in the embedded
//     set, it is deleted. This prevents retired built-ins from continuing to
//     match triggers with stale prompts or sidecar contracts.
//   - If a stored version exists with Builtin=false (user cleared the flag
//     to opt out of sync), it is preserved.
func SyncBuiltins(store *Store) error {
	defs, err := BuiltinDefinitions()
	if err != nil {
		return err
	}
	current := make(map[string]struct{}, len(defs))
	for i := range defs {
		current[defs[i].ID] = struct{}{}
	}
	stored, err := store.List()
	if err != nil {
		return fmt.Errorf("list workflows for builtin sync: %w", err)
	}
	var pruneErr error
	for i := range stored {
		if !stored[i].Builtin {
			continue
		}
		if _, ok := current[stored[i].ID]; ok {
			continue
		}
		if dErr := store.Delete(stored[i].ID); dErr != nil {
			pruneErr = errors.Join(pruneErr, fmt.Errorf("prune obsolete builtin %s: %w", stored[i].ID, dErr))
		}
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
	return pruneErr
}

// builtinsEqual compares two definitions ignoring timestamps. Timestamps are
// set by Save() each write, so byte-level comparison would always diverge.
func builtinsEqual(a, b Definition) bool {
	ah, err := a.SemanticHash()
	if err != nil {
		return false
	}
	bh, err := b.SemanticHash()
	if err != nil {
		return false
	}
	return ah == bh
}
