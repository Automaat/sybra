package abtest

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/skillinvoke"
)

// Select deterministically chooses a variant for the task/stage. It returns
// ok=false when A/B is disabled or no enabled experiment matches the role.
func Select(cfg Config, taskID, role, stepID string) (Assignment, bool, error) {
	return SelectEligible(cfg, taskID, role, stepID, nil)
}

// SelectionContext identifies the workflow step currently being assigned.
type SelectionContext struct {
	TaskID     string
	WorkflowID string
	Role       string
	StepID     string
	Prompt     string
}

// SelectEligible is Select with an optional provider eligibility predicate.
// Ineligible positive-weight variants are excluded before hashing so machines
// without a CLI do not silently fall back to a different provider while keeping
// stale experiment attribution.
func SelectEligible(cfg Config, taskID, role, stepID string, providerAllowed func(string) bool) (Assignment, bool, error) {
	return SelectEligibleForContext(cfg, SelectionContext{TaskID: taskID, Role: role, StepID: stepID}, providerAllowed)
}

// SelectEligibleForContext is SelectEligible with workflow subject context.
func SelectEligibleForContext(cfg Config, ctx SelectionContext, providerAllowed func(string) bool) (Assignment, bool, error) {
	if !cfg.EnabledValue() {
		return Assignment{}, false, nil
	}
	for i := range cfg.Experiments {
		exp := cfg.Experiments[i]
		if !exp.EnabledValue() || !roleMatches(exp.Roles, ctx.Role) || !subjectMatches(exp.Subject, ctx) {
			continue
		}
		return selectFromExperiment(exp, ctx.TaskID, ctx.Role, ctx.StepID, providerAllowed)
	}
	return Assignment{}, false, nil
}

func selectFromExperiment(exp Experiment, taskID, role, stepID string, providerAllowed func(string) bool) (Assignment, bool, error) {
	if err := validateExperiment(exp, providerAllowed); err != nil {
		return Assignment{}, false, err
	}
	unit := exp.AssignmentUnit
	if unit == "" {
		unit = "stage"
	}
	key := taskID
	if unit == "stage" {
		key = strings.Join([]string{taskID, role, stepID}, "|")
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
				Kind:            exp.KindValue(),
				VariantID:       v.ID,
				Provider:        v.Provider,
				Model:           v.Model,
				ReasoningEffort: v.ReasoningEffort,
				AssignmentUnit:  unit,
				AssignmentKey:   key,
				PromptTransform: clonePromptTransform(v.PromptTransform),
				SkillAliases:    cloneSkillAliases(v.SkillAliases),
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

func subjectMatches(subject *Subject, ctx SelectionContext) bool {
	if subject == nil {
		return true
	}
	if want := strings.TrimSpace(subject.WorkflowID); want != "" && want != ctx.WorkflowID {
		return false
	}
	if want := strings.TrimSpace(subject.StepID); want != "" && want != ctx.StepID {
		return false
	}
	if want := strings.TrimSpace(subject.Role); want != "" && want != defaultRole(ctx.Role) {
		return false
	}
	if want := strings.TrimSpace(subject.SkillName); want != "" && !skillinvoke.ContainsInvocation(ctx.Prompt, want) {
		return false
	}
	return true
}

func defaultRole(role string) string {
	if role == "" {
		return "implementation"
	}
	return role
}

func validateExperiment(exp Experiment, providerAllowed func(string) bool) error {
	if strings.TrimSpace(exp.ID) == "" {
		return fmt.Errorf("abtest: experiment id is required")
	}
	switch exp.KindValue() {
	case "model", "prompt", "skill", "compound":
	default:
		return fmt.Errorf("abtest: experiment %q has invalid kind %q", exp.ID, exp.Kind)
	}
	switch exp.AssignmentUnit {
	case "", "stage", "task":
	default:
		return fmt.Errorf("abtest: experiment %q has invalid assignment_unit %q", exp.ID, exp.AssignmentUnit)
	}
	bracket := strings.TrimSpace(exp.Bracket)
	switch bracket {
	case "", "cheap", "expensive":
	default:
		return fmt.Errorf("abtest: experiment %q has invalid bracket %q", exp.ID, exp.Bracket)
	}
	if err := validateExperimentSubject(exp); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range exp.Variants {
		v := exp.Variants[i]
		if v.Weight <= 0 {
			continue
		}
		if err := validateVariant(exp.ID, v); err != nil {
			return err
		}
		if bracket != "" && v.Tier != bracket {
			return fmt.Errorf("abtest: experiment %q bracket %q but variant %q has tier %q", exp.ID, bracket, v.ID, v.Tier)
		}
		if seen[v.ID] {
			return fmt.Errorf("abtest: experiment %q has duplicate variant id %q", exp.ID, v.ID)
		}
		seen[v.ID] = true
	}
	if err := validatePromptSkillHomogeneity(exp, providerAllowed); err != nil {
		return err
	}
	return nil
}

func validateExperimentSubject(exp Experiment) error {
	switch exp.KindValue() {
	case "prompt", "skill":
	default:
		return nil
	}
	if exp.Subject == nil {
		return fmt.Errorf("abtest: experiment %q kind %q requires subject", exp.ID, exp.KindValue())
	}
	if strings.TrimSpace(exp.Subject.WorkflowID) == "" && strings.TrimSpace(exp.Subject.StepID) == "" && strings.TrimSpace(exp.Subject.Role) == "" {
		return fmt.Errorf("abtest: experiment %q kind %q requires subject workflow_id, step_id, or role", exp.ID, exp.KindValue())
	}
	if exp.KindValue() == "skill" && strings.TrimSpace(exp.Subject.SkillName) == "" {
		return fmt.Errorf("abtest: experiment %q kind %q requires subject skill_name", exp.ID, exp.KindValue())
	}
	return nil
}

func validatePromptSkillHomogeneity(exp Experiment, providerAllowed func(string) bool) error {
	switch exp.KindValue() {
	case "prompt", "skill":
	default:
		return nil
	}
	eligible, _ := EligibleVariants(exp, providerAllowed)
	if len(eligible) < 2 {
		return nil
	}
	base := eligible[0]
	for i := 1; i < len(eligible); i++ {
		v := eligible[i]
		if v.Provider != base.Provider {
			return fmt.Errorf("abtest: experiment %q provider mismatch on variant %q", exp.ID, v.ID)
		}
		if v.Model != base.Model {
			return fmt.Errorf("abtest: experiment %q model mismatch on variant %q", exp.ID, v.ID)
		}
		if v.ReasoningEffort != base.ReasoningEffort {
			return fmt.Errorf("abtest: experiment %q reasoning_effort mismatch on variant %q", exp.ID, v.ID)
		}
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
	switch v.Tier {
	case "", "cheap", "expensive":
	default:
		return fmt.Errorf("abtest: variant %q has invalid tier %q", v.ID, v.Tier)
	}
	if err := validatePromptTransform(v); err != nil {
		return err
	}
	if err := validateSkillAliases(v); err != nil {
		return err
	}
	return nil
}

func validatePromptTransform(v Variant) error {
	if v.PromptTransform == nil {
		return nil
	}
	op := strings.TrimSpace(v.PromptTransform.Op)
	text := strings.TrimSpace(v.PromptTransform.Text)
	switch op {
	case "":
		return nil
	case "replace", "template":
		if text == "" {
			return fmt.Errorf("abtest: variant %q prompt_transform %q requires text", v.ID, op)
		}
	case "prepend", "append":
	default:
		return fmt.Errorf("abtest: variant %q has invalid prompt_transform op %q", v.ID, v.PromptTransform.Op)
	}
	return nil
}

func validateSkillAliases(v Variant) error {
	for from, to := range v.SkillAliases {
		if from != strings.TrimSpace(from) {
			return fmt.Errorf("abtest: variant %q has invalid skill alias source %q", v.ID, from)
		}
		if to != strings.TrimSpace(to) {
			return fmt.Errorf("abtest: variant %q has invalid skill alias target %q", v.ID, to)
		}
		normalizedFrom, ok := skillinvoke.NormalizeName(from)
		if !ok || normalizedFrom != strings.TrimPrefix(strings.TrimSpace(from), "/") {
			return fmt.Errorf("abtest: variant %q has invalid skill alias source %q", v.ID, from)
		}
		normalizedTo, ok := skillinvoke.NormalizeName(to)
		if !ok || normalizedTo != strings.TrimPrefix(strings.TrimSpace(to), "/") {
			return fmt.Errorf("abtest: variant %q has invalid skill alias target %q", v.ID, to)
		}
	}
	return nil
}

func clonePromptTransform(in *PromptTransform) *PromptTransform {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneSkillAliases(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for from, to := range in {
		normalizedFrom, okFrom := skillinvoke.NormalizeName(from)
		normalizedTo, okTo := skillinvoke.NormalizeName(to)
		if !okFrom || !okTo {
			continue
		}
		out[normalizedFrom] = normalizedTo
	}
	return out
}

func hashKey(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
