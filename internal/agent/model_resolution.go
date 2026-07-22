package agent

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/modeltier"
)

var ErrProviderModelIncompatible = errors.New("provider/model incompatible after failover")

const providerModelIncompatibleErrorKind = "provider_model_incompatible"

type ProviderModelIncompatibleError struct {
	RequestedProvider string
	SelectedProvider  string
	Model             string
}

func (e *ProviderModelIncompatibleError) Error() string {
	if e == nil {
		return ErrProviderModelIncompatible.Error()
	}
	return fmt.Sprintf("%s: provider=%q selected=%q model=%q",
		ErrProviderModelIncompatible, e.RequestedProvider, e.SelectedProvider, e.Model)
}

func (e *ProviderModelIncompatibleError) Unwrap() error { return ErrProviderModelIncompatible }

func resolveRunModel(requestedProvider, selectedProvider, model string) (resolvedModel, nextRequestedModel string, err error) {
	prov, err := lookupProvider(selectedProvider)
	if err != nil {
		return "", model, err
	}
	raw := strings.TrimSpace(model)
	if selectedProvider == requestedProvider {
		return prov.NormalizeModel(raw), raw, nil
	}
	if tier, ok := modeltier.InferTier(stripContextSuffix(raw)); ok {
		mapped := modeltier.Model(tier, selectedProvider)
		if strings.TrimSpace(mapped) == "" {
			return "", raw, &ProviderModelIncompatibleError{
				RequestedProvider: requestedProvider,
				SelectedProvider:  selectedProvider,
				Model:             raw,
			}
		}
		return mapped, modeltier.Alias(tier), nil
	}
	return "", raw, &ProviderModelIncompatibleError{
		RequestedProvider: requestedProvider,
		SelectedProvider:  selectedProvider,
		Model:             raw,
	}
}

func resolvedModelForRun(cfg RunConfig, prov Provider) (string, error) {
	if cfg.resolvedModel != "" {
		return cfg.resolvedModel, nil
	}
	model, _, err := resolveRunModel(normalizeProvider(cfg.Provider), prov.Name(), cfg.Model)
	if err != nil {
		return "", err
	}
	return model, nil
}
