package sybra

import (
	"errors"
	"os"

	"github.com/Automaat/sybra/internal/config"
)

type ConfigPathExplanation struct {
	Descriptor     config.PathDescriptor `json:"descriptor"`
	Default        config.PathValue      `json:"default"`
	Intent         config.PathValue      `json:"intent"`
	Effective      config.PathValue      `json:"effective"`
	Override       *config.PathValue     `json:"override,omitempty"`
	ReloadPolicy   configReloadPolicy    `json:"reloadPolicy"`
	Visibility     configVisibility      `json:"visibility"`
	PendingRestart bool                  `json:"pendingRestart"`
}

func LoadConfigPathExplanations(active, persisted *config.Config) ([]ConfigPathExplanation, error) {
	fileCfg, err := currentFileConfig()
	if err != nil {
		return nil, err
	}
	env := config.CurrentEnvironment()
	activeCfg := active
	if activeCfg == nil {
		activeCfg = config.DefaultConfig()
	}
	persistedCfg := persisted
	if persistedCfg == nil {
		persistedCfg = activeCfg
	}

	activeExplanations := config.ExplainAll(fileCfg, env, activeCfg)

	out := make([]ConfigPathExplanation, 0, len(activeExplanations))
	for _, explanation := range activeExplanations {
		meta, ok := ConfigRegistryMetadataByRuntimePath(explanation.Descriptor.RuntimePath)
		if !ok {
			continue
		}
		pendingRestart := meta.Policy == configPolicyRestart &&
			!configValuesEqual(
				configValueAtPath(*activeCfg, explanation.Descriptor.RuntimePath),
				configValueAtPath(*persistedCfg, explanation.Descriptor.RuntimePath),
			)

		out = append(out, ConfigPathExplanation{
			Descriptor:     explanation.Descriptor,
			Default:        explanation.Default,
			Intent:         explanation.Intent,
			Effective:      explanation.Effective,
			Override:       explanation.Override,
			ReloadPolicy:   meta.Policy,
			Visibility:     meta.Visibility,
			PendingRestart: pendingRestart,
		})
	}
	return out, nil
}

func LoadConfigPathExplanation(path string, active, persisted *config.Config) (ConfigPathExplanation, error) {
	runtimePath, ok := config.NormalizeRuntimeYAMLPath(path)
	if !ok {
		return ConfigPathExplanation{}, errors.New(configUnknownPathError(path))
	}
	explanations, err := LoadConfigPathExplanations(active, persisted)
	if err != nil {
		return ConfigPathExplanation{}, err
	}
	for _, explanation := range explanations {
		if explanation.Descriptor.RuntimePath == runtimePath {
			return explanation, nil
		}
	}
	return ConfigPathExplanation{}, errors.New(configUnknownPathError(path))
}

func currentFileConfig() (*config.FileConfig, error) {
	raw, err := config.ReadRawConfig()
	switch {
	case err == nil:
		return config.ParseFileConfig([]byte(raw))
	case os.IsNotExist(err):
		return nil, nil
	default:
		return nil, err
	}
}

func configUnknownPathError(path string) string {
	_, err := config.ExplainPath(path, nil, config.Environment{}, config.DefaultConfig())
	if err != nil {
		return err.Error()
	}
	return "unknown config path " + path
}
