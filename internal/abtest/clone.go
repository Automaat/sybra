package abtest

import (
	"maps"
	"slices"
)

// CloneConfig returns a deep copy of an A/B config suitable for cross-goroutine
// publication without sharing mutable slice, map, or pointer fields.
func CloneConfig(src Config) Config {
	cp := src
	if src.Enabled != nil {
		v := *src.Enabled
		cp.Enabled = &v
	}
	if src.BuiltinVersion != nil {
		v := *src.BuiltinVersion
		cp.BuiltinVersion = &v
	}
	if src.WeightsVersion != nil {
		v := *src.WeightsVersion
		cp.WeightsVersion = &v
	}
	cp.Experiments = make([]Experiment, len(src.Experiments))
	for i := range src.Experiments {
		exp := src.Experiments[i]
		if exp.Enabled != nil {
			v := *exp.Enabled
			exp.Enabled = &v
		}
		if exp.Subject != nil {
			subj := *exp.Subject
			exp.Subject = &subj
		}
		exp.Roles = slices.Clone(exp.Roles)
		exp.Variants = make([]Variant, len(src.Experiments[i].Variants))
		for j := range src.Experiments[i].Variants {
			variant := src.Experiments[i].Variants[j]
			if variant.PromptTransform != nil {
				transform := *variant.PromptTransform
				variant.PromptTransform = &transform
			}
			variant.SkillAliases = maps.Clone(variant.SkillAliases)
			exp.Variants[j] = variant
		}
		cp.Experiments[i] = exp
	}
	return cp
}
