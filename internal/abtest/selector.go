package abtest

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/skillinvoke"
)

// defaultReasoningEffort mirrors agent.DefaultReasoningEffort. It cannot be
// imported directly: internal/agent transitively depends on internal/config,
// which depends on internal/abtest, so importing agent here would cycle.
const defaultReasoningEffort = "medium"

// Select deterministically chooses a variant for the task/stage. It returns
// ok=false when A/B is disabled or no enabled experiment matches the role.
func Select(cfg Config, taskID, role, stepID string) (Assignment, bool, error) {
	return SelectEligible(cfg, taskID, role, stepID, nil)
}

// EvalPassed reports whether a variant's offline eval verdict allows online
// enrollment (e.g. internal/prompteval.Gate.AllowEnrollment). Implementations
// must only read a precomputed verdict — never run an eval on this call path.
type EvalPassed func(variantID, digest string) bool

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
	return SelectEligibleWithEval(cfg, taskID, role, stepID, providerAllowed, nil)
}

// SelectEligibleWithEval is SelectEligible with an additional nil-safe
// offline-eval predicate. A positive-weight variant is skipped only when
// evalPassed is non-nil AND the variant carries a non-empty Digest AND
// evalPassed returns false — so callers that pass nil (or variants without a
// digest) see byte-for-byte unchanged selection.
func SelectEligibleWithEval(cfg Config, taskID, role, stepID string, providerAllowed func(string) bool, evalPassed EvalPassed) (Assignment, bool, error) {
	return SelectEligibleForContext(cfg, SelectionContext{TaskID: taskID, Role: role, StepID: stepID}, providerAllowed, evalPassed)
}

// SelectEligibleForContext is SelectEligible with workflow subject context.
// Equivalent to SelectEligibleForContextWithCohort with a nil cohortObserved
// — no experiment in cfg is treated as canary-gated even if it declares a
// Canary policy (see that function's fail-closed-to-baseline doc comment).
func SelectEligibleForContext(cfg Config, ctx SelectionContext, providerAllowed func(string) bool, evalPassed EvalPassed) (Assignment, bool, error) {
	return SelectEligibleForContextWithCohort(cfg, ctx, providerAllowed, evalPassed, nil)
}

// CohortObserved reports the resolved-run count and freshness backing an
// experiment's canary gate — typically sourced from the evaluation
// scorecard's per-experiment sample size and evaluation.Trustworthy. abtest
// has no notion of time-based staleness on its own; fresh=false is treated
// exactly like an insufficient cohort. Never called for an experiment
// without a Canary policy.
type CohortObserved func(experimentID string) (observed int, fresh bool)

// SelectEligibleForContextWithCohort is SelectEligibleForContext with an
// additional canary-cohort predicate. An experiment with a non-nil Canary
// policy forces every assignment to its BaselineVariantID unless
// cohortObserved reports a fresh cohort of at least MinCohort resolved runs
// — and even then, bounds non-baseline-eligible traffic to PercentBound via
// the same deterministic per-(task,role,stage) hash the normal weighted
// draw uses, so canary membership is stable and reproducible. A canary
// experiment given a nil cohortObserved fails closed to its baseline (no
// observability, no canary traffic) — the same fail-closed posture this
// package already applies to positive-weight-only variants and provider
// eligibility.
func SelectEligibleForContextWithCohort(cfg Config, ctx SelectionContext, providerAllowed func(string) bool, evalPassed EvalPassed, cohortObserved CohortObserved) (Assignment, bool, error) {
	if !cfg.EnabledValue() {
		return Assignment{}, false, nil
	}
	for i := range cfg.Experiments {
		exp := cfg.Experiments[i]
		if !exp.EnabledValue() || !roleMatches(exp.Roles, ctx.Role) || !subjectMatches(exp.Subject, ctx) {
			continue
		}
		if exp.Canary != nil {
			return selectFromCanaryExperiment(exp, ctx.TaskID, ctx.Role, ctx.StepID, providerAllowed, evalPassed, cfg.WeightsVersion, cohortObserved)
		}
		return selectFromExperiment(exp, ctx.TaskID, ctx.Role, ctx.StepID, providerAllowed, evalPassed, cfg.WeightsVersion)
	}
	return Assignment{}, false, nil
}

// selectFromCanaryExperiment applies exp.Canary's cohort/percent gate before
// falling back to the normal weighted draw. Returns ok=false (no error) when
// the baseline variant itself is not providerAllowed, deferring to plain
// (non-AB) provider selection and failover exactly like selectFromExperiment
// does when every variant is ineligible — a canary must never hard-error
// dispatch just because its baseline provider is temporarily unavailable;
// that is gateProvider's job, independently, at dispatch time.
func selectFromCanaryExperiment(exp Experiment, taskID, role, stepID string, providerAllowed func(string) bool, evalPassed EvalPassed, weightsVersion *int, cohortObserved CohortObserved) (Assignment, bool, error) {
	if err := validateExperiment(exp, providerAllowed); err != nil {
		return Assignment{}, false, err
	}
	baseline, ok := findVariant(exp, exp.Canary.BaselineVariantID)
	if !ok {
		return Assignment{}, false, fmt.Errorf("abtest: experiment %q canary baseline_variant_id %q not found", exp.ID, exp.Canary.BaselineVariantID)
	}
	if providerAllowed != nil && !providerAllowed(baseline.Provider) {
		return Assignment{}, false, nil
	}

	unit := exp.AssignmentUnit
	if unit == "" {
		unit = "stage"
	}
	key := taskID
	if unit == "stage" {
		key = strings.Join([]string{taskID, role, stepID}, "|")
	}

	observed, fresh := 0, false
	if cohortObserved != nil {
		observed, fresh = cohortObserved(exp.ID)
	}
	inCohort := fresh && observed >= exp.Canary.MinCohort
	// Separate hash namespace ("canary|") from the variant-pick hash below
	// so canary-bucket membership and (once admitted) variant choice are
	// independent draws for the same key.
	inCanaryBucket := hashKey("canary|"+exp.ID+"|"+key)%100 < clampPercent(exp.Canary.PercentBound)
	if !inCohort || !inCanaryBucket {
		// The baseline is still an online enrollment, so it must clear the same
		// offline-eval gate as any weighted draw. A digested baseline whose
		// verdict failed must not enroll just because the cohort is cold or the
		// request fell outside PercentBound — fall out of the experiment and
		// defer to plain (non-AB) provider selection, matching the
		// provider-ineligible baseline path above.
		if evalPassed != nil && baseline.Digest != "" && !evalPassed(baseline.ID, baseline.Digest) {
			return Assignment{}, false, nil
		}
		return baselineAssignment(exp, baseline, unit, key, weightsVersion), true, nil
	}
	return selectFromExperiment(exp, taskID, role, stepID, providerAllowed, evalPassed, weightsVersion)
}

// clampPercent bounds p to [0,100] and returns it as a uint64, matching
// hashKey's modulo result type — validateCanary already rejects an
// out-of-range PercentBound before selection, this is a defensive clamp.
func clampPercent(p int) uint64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	// #nosec G115 -- bounded to [0,100] above; uint64 cannot overflow.
	return uint64(p)
}

func baselineAssignment(exp Experiment, v Variant, unit, key string, weightsVersion *int) Assignment {
	decisionVersion := 0
	if weightsVersion != nil {
		decisionVersion = *weightsVersion
	}
	return Assignment{
		ExperimentID:    exp.ID,
		Kind:            exp.KindValue(),
		VariantID:       v.ID,
		RoutingReason:   "canary_baseline",
		Provider:        v.Provider,
		Model:           v.Model,
		ReasoningEffort: v.ReasoningEffort,
		AssignmentUnit:  unit,
		AssignmentKey:   key,
		PromptTransform: clonePromptTransform(v.PromptTransform),
		SkillAliases:    cloneSkillAliases(v.SkillAliases),
		DecisionVersion: decisionVersion,
	}
}

func selectFromExperiment(exp Experiment, taskID, role, stepID string, providerAllowed func(string) bool, evalPassed EvalPassed, weightsVersion *int) (Assignment, bool, error) {
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
	eligible, total := EligibleVariants(exp, providerAllowed, evalPassed)
	if total <= 0 {
		if providerAllowed != nil {
			// Zero eligible variants may mean every variant's provider is
			// currently unhealthy/rate-limited rather than a real config
			// error (e.g. all weights zero, or eval-gated out). Re-check
			// without the provider filter: if variants would otherwise be
			// eligible, this experiment simply has no healthy provider
			// right now — defer to normal (non-A/B) provider selection and
			// failover instead of hard-erroring the whole dispatch.
			_, unfilteredTotal := EligibleVariants(exp, nil, evalPassed)
			if unfilteredTotal > 0 {
				return Assignment{}, false, nil
			}
		}
		return Assignment{}, false, fmt.Errorf("abtest: experiment %q has no eligible positive-weight variants", exp.ID)
	}
	pick := hashKey(exp.ID+"|"+key) % uint64(total)
	var acc uint64
	for i := range eligible {
		v := eligible[i]
		// #nosec G115 -- positive weights are validated above; uint64 cannot overflow.
		acc += uint64(v.Weight)
		if pick < acc {
			decisionVersion := 0
			if weightsVersion != nil {
				decisionVersion = *weightsVersion
			}
			return Assignment{
				ExperimentID:    exp.ID,
				Kind:            exp.KindValue(),
				VariantID:       v.ID,
				RoutingReason:   "ab",
				Provider:        v.Provider,
				Model:           v.Model,
				ReasoningEffort: v.ReasoningEffort,
				AssignmentUnit:  unit,
				AssignmentKey:   key,
				PromptTransform: clonePromptTransform(v.PromptTransform),
				SkillAliases:    cloneSkillAliases(v.SkillAliases),
				DecisionVersion: decisionVersion,
			}, true, nil
		}
	}
	return Assignment{}, false, fmt.Errorf("abtest: experiment %q selection fell through", exp.ID)
}

// EligibleVariants returns the positive-weight variants that can be assigned.
// A variant is skipped when evalPassed is non-nil AND it carries a non-empty
// Digest AND evalPassed(v.ID, v.Digest) returns false — evalPassed=nil or a
// digest-less variant (e.g. model-only experiments) never changes the result.
func EligibleVariants(exp Experiment, providerAllowed func(string) bool, evalPassed EvalPassed) (eligible []Variant, totalWeight int) {
	eligible = make([]Variant, 0, len(exp.Variants))
	for i := range exp.Variants {
		v := exp.Variants[i]
		if v.Weight <= 0 {
			continue
		}
		if providerAllowed != nil && !providerAllowed(v.Provider) {
			continue
		}
		if evalPassed != nil && v.Digest != "" && !evalPassed(v.ID, v.Digest) {
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
	if err := validateCanary(exp); err != nil {
		return err
	}
	return nil
}

func validateCanary(exp Experiment) error {
	c := exp.Canary
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.BaselineVariantID) == "" {
		return fmt.Errorf("abtest: experiment %q canary requires baseline_variant_id", exp.ID)
	}
	if c.PercentBound < 0 || c.PercentBound > 100 {
		return fmt.Errorf("abtest: experiment %q canary percent_bound %d out of range [0,100]", exp.ID, c.PercentBound)
	}
	if c.MinCohort < 0 {
		return fmt.Errorf("abtest: experiment %q canary min_cohort must be >= 0", exp.ID)
	}
	if _, ok := findVariant(exp, c.BaselineVariantID); !ok {
		return fmt.Errorf("abtest: experiment %q canary baseline_variant_id %q not found among variants", exp.ID, c.BaselineVariantID)
	}
	return nil
}

func findVariant(exp Experiment, id string) (Variant, bool) {
	for i := range exp.Variants {
		if exp.Variants[i].ID == id {
			return exp.Variants[i], true
		}
	}
	return Variant{}, false
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
	eligible, _ := EligibleVariants(exp, providerAllowed, nil)
	if len(eligible) < 2 {
		return nil
	}
	base := eligible[0]
	baseEffort := normalizeReasoningEffort(base.ReasoningEffort)
	for i := 1; i < len(eligible); i++ {
		v := eligible[i]
		if v.Provider != base.Provider {
			return fmt.Errorf("abtest: experiment %q provider mismatch on variant %q", exp.ID, v.ID)
		}
		if v.Model != base.Model {
			return fmt.Errorf("abtest: experiment %q model mismatch on variant %q", exp.ID, v.ID)
		}
		if normalizeReasoningEffort(v.ReasoningEffort) != baseEffort {
			return fmt.Errorf("abtest: experiment %q reasoning_effort mismatch on variant %q", exp.ID, v.ID)
		}
	}
	return nil
}

// normalizeReasoningEffort treats an omitted effort as the agent runtime's
// default, so a variant that explicitly sets "medium" is homogeneous with one
// that leaves the field empty.
func normalizeReasoningEffort(effort string) string {
	if effort == "" {
		return defaultReasoningEffort
	}
	return effort
}

func validateVariant(expID string, v Variant) error {
	if strings.TrimSpace(v.ID) == "" {
		return fmt.Errorf("abtest: experiment %q has variant with empty id", expID)
	}
	if !providerid.IsKnown(v.Provider) {
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
