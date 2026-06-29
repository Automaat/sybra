package abtest

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
)

// Select deterministically chooses a variant for the task/stage. It returns
// ok=false when A/B is disabled or no enabled experiment matches the role.
func Select(cfg Config, taskID, role, stepID string) (Assignment, bool, error) {
	return SelectEligible(cfg, taskID, role, stepID, nil)
}

// SelectEligible is Select with an optional provider eligibility predicate.
// Ineligible positive-weight variants are excluded before hashing so machines
// without a CLI do not silently fall back to a different provider while keeping
// stale experiment attribution.
func SelectEligible(cfg Config, taskID, role, stepID string, providerAllowed func(string) bool) (Assignment, bool, error) {
	if !cfg.EnabledValue() {
		return Assignment{}, false, nil
	}
	for i := range cfg.Experiments {
		exp := cfg.Experiments[i]
		if !exp.EnabledValue() || !roleMatches(exp.Roles, role) {
			continue
		}
		return selectFromExperiment(exp, taskID, role, stepID, providerAllowed)
	}
	return Assignment{}, false, nil
}

func selectFromExperiment(exp Experiment, taskID, role, stepID string, providerAllowed func(string) bool) (Assignment, bool, error) {
	if strings.TrimSpace(exp.ID) == "" {
		return Assignment{}, false, fmt.Errorf("abtest: experiment id is required")
	}
	if err := validateExperiment(exp); err != nil {
		return Assignment{}, false, err
	}
	unit := exp.AssignmentUnit
	if unit == "" {
		unit = "stage"
	}
	key := taskID
	if unit == "stage" {
		key = strings.Join([]string{taskID, role, stepID}, "|")
	} else if unit != "task" {
		return Assignment{}, false, fmt.Errorf("abtest: experiment %q has invalid assignment_unit %q", exp.ID, exp.AssignmentUnit)
	}
	eligible, total := EligibleVariants(exp, providerAllowed)
	if total <= 0 {
		return Assignment{}, false, fmt.Errorf("abtest: experiment %q has no eligible positive-weight variants", exp.ID)
	}
	pick := hashKey(exp.ID+"|"+key) % uint64(total)
	var acc uint64
	for i := range eligible {
		v := eligible[i]
		// #nosec G115 -- positive weights are validated above; uint64 cannot overflow.
		acc += uint64(v.Weight)
		if pick < acc {
			return Assignment{
				ExperimentID:    exp.ID,
				VariantID:       v.ID,
				Provider:        v.Provider,
				Model:           v.Model,
				ReasoningEffort: v.ReasoningEffort,
				AssignmentUnit:  unit,
				AssignmentKey:   key,
			}, true, nil
		}
	}
	return Assignment{}, false, fmt.Errorf("abtest: experiment %q selection fell through", exp.ID)
}

// EligibleVariants returns the positive-weight variants that can be assigned.
func EligibleVariants(exp Experiment, providerAllowed func(string) bool) (eligible []Variant, totalWeight int) {
	eligible = make([]Variant, 0, len(exp.Variants))
	for i := range exp.Variants {
		v := exp.Variants[i]
		if v.Weight <= 0 {
			continue
		}
		if providerAllowed != nil && !providerAllowed(v.Provider) {
			continue
		}
		totalWeight += v.Weight
		eligible = append(eligible, v)
	}
	return eligible, totalWeight
}

func roleMatches(roles []string, role string) bool {
	if len(roles) == 0 {
		return true
	}
	if role == "" {
		role = "implementation"
	}
	return slices.Contains(roles, role)
}

func validateExperiment(exp Experiment) error {
	seen := map[string]bool{}
	for i := range exp.Variants {
		v := exp.Variants[i]
		if v.Weight <= 0 {
			continue
		}
		if err := validateVariant(exp.ID, v); err != nil {
			return err
		}
		if seen[v.ID] {
			return fmt.Errorf("abtest: experiment %q has duplicate variant id %q", exp.ID, v.ID)
		}
		seen[v.ID] = true
	}
	return nil
}

func validateVariant(expID string, v Variant) error {
	if strings.TrimSpace(v.ID) == "" {
		return fmt.Errorf("abtest: experiment %q has variant with empty id", expID)
	}
	switch v.Provider {
	case "claude", "codex", "copilot":
	default:
		return fmt.Errorf("abtest: variant %q has invalid provider %q", v.ID, v.Provider)
	}
	if strings.TrimSpace(v.Model) == "" {
		return fmt.Errorf("abtest: variant %q must set model explicitly", v.ID)
	}
	return nil
}

func hashKey(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
