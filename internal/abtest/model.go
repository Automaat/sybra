package abtest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Automaat/sybra/internal/modeltier"
	"github.com/Automaat/sybra/internal/providerid"
)

// Config controls deterministic A/B assignment for workflow agent runs.
type Config struct {
	Enabled              *bool        `yaml:"enabled" json:"enabled"`
	MinSamplesPerVariant int          `yaml:"min_samples_per_variant" json:"minSamplesPerVariant"`
	Experiments          []Experiment `yaml:"experiments" json:"experiments"`
	// BuiltinVersion stamps which generation of DefaultConfig's built-in
	// experiments a persisted config has adopted. config.applyABTestingDefaults
	// bumps a lagging config's built-in experiments (see BuiltinExperimentIDs)
	// up to CurrentBuiltinVersion, leaving any other (user-authored) experiment
	// untouched.
	BuiltinVersion *int `yaml:"builtin_version,omitempty" json:"builtinVersion,omitempty"`
	// WeightsVersion stamps the routing-service generation that produced this
	// config's variant weights — nil for a plain operator-authored config
	// (weights come straight from config.yaml, no overlay applied). Copied
	// onto every Assignment as DecisionVersion so a run record can be joined
	// back to the routing.reweighted audit event that set its weights. Never
	// persisted to config.yaml — internal/routing sets it only on the
	// in-memory merged config it pushes to selection sites.
	WeightsVersion *int `yaml:"-" json:"weightsVersion,omitempty"`
}

// CurrentBuiltinVersion is the version stamp for the built-in experiment set
// returned by DefaultConfig. Bump this whenever the built-in experiments'
// roles, variants, or weights change in a way that persisted configs should
// pick up automatically.
const CurrentBuiltinVersion = 8

// BuiltinExperimentIDs lists the experiment IDs owned by Sybra's shipped
// defaults. A persisted config's experiment is only replaced during a builtin
// reconcile if its ID appears here — every other experiment ID is treated as
// user-authored and preserved verbatim.
var BuiltinExperimentIDs = []string{
	"code-author-cheap",
	"code-author-maintenance-cheap",
	"fix-review-expensive",
	"review-expensive",
	"review-tighten-instructions-pl-a2d853b2c1d9",
	"human-review-restructure-context-pl-50bbd0314913",
}

// HumanReviewRestructureContextPL50BBD0314913 is Prompt Lab proposal
// pl-50bbd0314913's candidate text for the human-review role: prepended to
// the base human-review prompt (internal/sybra buildPrompt), it tells the
// reviewer to read the deterministic task/workflow/run signals before the
// free-text agent results and host log tail, so noisy log prose cannot
// override a conclusion the structured signals already establish. Its sha256
// digest (see the pl-50bbd0314913 variant below) must match the exact bytes
// screened by `sybra-cli evaluation offline run` — editing this text
// invalidates that verdict and re-blocks enrollment until the offline eval is
// re-run.
const HumanReviewRestructureContextPL50BBD0314913 = `
## Context reading order (apply before deciding)

Read the context below in this fixed priority order, not the order it appears:

1. Task status, status_reason, and Workflow execution (current step) — the deterministic record of where and why the task stalled.
2. Each recent agent run's deterministic metadata: state, protocol_violation, test_outcome, test_failure_fingerprint — trust these over any prose in that run's Result block.
3. Only then read agent Result text and the Sybra host log tail, and only as far as needed to explain what steps 1-2 already narrowed down.

If steps 1-2 already establish a clear stall reason — a genuine human-required cause, or a Sybra bug pattern — do not let unrelated noise later in the log tail override that conclusion. In "reason", name which of steps 1-2 grounded your decision before citing any log excerpt.
`

const ReviewTightenInstructionsPLA2D853B2C1D9 = `

Review variant pl-a2d853b2c1d9: apply a stricter staff-code-review standard before returning a verdict.

- Lead with concrete blocking defects only: correctness, security, data loss, broken workflows, test gaps that can hide a regression, or repository-rule violations.
- For every finding, name the exact file/line evidence and the user-visible failure mode. If you cannot tie a concern to behavior or a repo rule, omit it.
- Check that implementation and tests cover the task's current acceptance criteria, including edge cases and failure paths. Treat missing focused tests as a finding when the change is risky or user-visible.
- Separate must-fix-before-merge issues from optional cleanup; do not block on style preferences, speculative rewrites, or unrelated refactors.
- If no blocking issues remain, say that clearly and call out any residual test or runtime verification gap.
`

const ImplementationTightenInstructionsPL41673AA95495 = `

Implementation variant pl-41673aa95495: tighten completion and verification discipline before finishing.

- Restate the task's acceptance criteria (and plan contract, if any) as an explicit checklist before writing code; if a criterion is ambiguous or unverifiable, stop and mark the task human-required with the specific blocker instead of guessing.
- Touch only the files needed to satisfy those criteria — no unrelated refactors, renames, or "while I'm here" cleanup that risks an unreviewed regression.
- For every edge case implied by the task (empty/nil input, error paths, boundary values, concurrent access), add or run a focused test that exercises it before considering the work done.
- After committing, confirm the push actually landed and the branch is ahead of origin/main — a task with no pushed commit is not finished.
- Before finishing, re-check the actual diff against every acceptance criterion one by one; do not rely on memory of what you intended to write.
`

func digestString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Experiment selects among variants for matching workflow roles.
type Experiment struct {
	ID             string    `yaml:"id" json:"id"`
	Kind           string    `yaml:"kind,omitempty" json:"kind,omitempty"`
	Enabled        *bool     `yaml:"enabled" json:"enabled"`
	AssignmentUnit string    `yaml:"assignment_unit" json:"assignmentUnit"`      // "task" or "stage"; empty defaults to "stage"
	Bracket        string    `yaml:"bracket,omitempty" json:"bracket,omitempty"` // "cheap" or "expensive"
	Subject        *Subject  `yaml:"subject,omitempty" json:"subject,omitempty"`
	Roles          []string  `yaml:"roles" json:"roles"`
	Variants       []Variant `yaml:"variants" json:"variants"`
	// Canary bounds and gates how much of this experiment's traffic may
	// deviate from its declared baseline variant. nil (the default) means
	// uncapped: every eligible variant competes for its declared Weight
	// share exactly as today. See CanaryPolicy and SelectEligibleForContextWithCohort.
	Canary *CanaryPolicy `yaml:"canary,omitempty" json:"canary,omitempty"`
}

// CanaryPolicy bounds and gates non-baseline traffic for an experiment,
// keeping production routing on a stable baseline until a canary has both
// enough observed data and a fresh (trustworthy) signal behind it.
type CanaryPolicy struct {
	// BaselineVariantID names the Variant every gated-out assignment
	// resolves to. Must match a configured Variant.ID.
	BaselineVariantID string `yaml:"baseline_variant_id" json:"baselineVariantId"`
	// PercentBound is the maximum share (0-100) of traffic eligible to
	// resolve to any variant at all (including the baseline itself, since
	// the baseline may also win the normal weighted draw); the rest is
	// forced to baseline. Bucketing uses the same deterministic hash as
	// variant selection, so a task/role's canary membership is stable and
	// reproducible across repeated selections.
	PercentBound int `yaml:"percent_bound" json:"percentBound"`
	// MinCohort is the minimum resolved-run count a CohortObserved call
	// must report for this experiment before any traffic is allowed outside
	// the forced-baseline slice, regardless of PercentBound. A cohort
	// predicate that reports fresh=false (e.g. the backing evaluation
	// report is stale/untrustworthy) is treated the same as an
	// insufficient cohort.
	MinCohort int `yaml:"min_cohort" json:"minCohort"`
}

// Subject identifies the workflow target for prompt and skill experiments.
type Subject struct {
	WorkflowID string `yaml:"workflow_id,omitempty" json:"workflowId,omitempty"`
	StepID     string `yaml:"step_id,omitempty" json:"stepId,omitempty"`
	Role       string `yaml:"role,omitempty" json:"role,omitempty"`
	SkillName  string `yaml:"skill_name,omitempty" json:"skillName,omitempty"`
}

// Variant is one provider/model arm of an experiment.
type Variant struct {
	ID              string            `yaml:"id" json:"id"`
	Provider        string            `yaml:"provider" json:"provider"`
	Model           string            `yaml:"model" json:"model"`
	ReasoningEffort string            `yaml:"reasoning_effort,omitempty" json:"reasoningEffort,omitempty"`
	Tier            string            `yaml:"tier,omitempty" json:"tier,omitempty"` // "cheap" or "expensive"
	Version         string            `yaml:"version,omitempty" json:"version,omitempty"`
	Digest          string            `yaml:"digest,omitempty" json:"digest,omitempty"`
	PromptTransform *PromptTransform  `yaml:"prompt_transform,omitempty" json:"promptTransform,omitempty"`
	SkillAliases    map[string]string `yaml:"skill_aliases,omitempty" json:"skillAliases,omitempty"`
	Weight          int               `yaml:"weight" json:"weight"`
}

// PromptTransform describes an optional variant-specific prompt rewrite.
type PromptTransform struct {
	Op   string `yaml:"op,omitempty" json:"op,omitempty"`
	Text string `yaml:"text,omitempty" json:"text,omitempty"`
}

// ApplyPromptTransformOpText rewrites prompt per op/text: "replace"/
// "template" overwrite it outright, "prepend"/"append" splice text in, any
// other value (including empty) leaves prompt unchanged. Op and text are
// taken as plain strings rather than *PromptTransform so dispatch paths that
// mirror PromptTransform as a local type (e.g. internal/workflow, to avoid
// depending on internal/abtest in their launcher interface) can still share
// this logic instead of reimplementing the switch.
func ApplyPromptTransformOpText(prompt, op, text string) string {
	switch strings.TrimSpace(op) {
	case "replace", "template":
		return text
	case "prepend":
		return text + prompt
	case "append":
		return prompt + text
	default:
		return prompt
	}
}

// Assignment is the durable identity selected for a workflow stage.
type Assignment struct {
	ExperimentID    string
	Kind            string
	VariantID       string
	RoutingReason   string
	Provider        string
	Model           string
	ReasoningEffort string
	AssignmentUnit  string
	AssignmentKey   string
	PromptTransform *PromptTransform
	SkillAliases    map[string]string
	// DecisionVersion mirrors the Config's WeightsVersion at selection time
	// (0 when the config carries no routing overlay), so downstream run
	// records can be attributed to the routing generation that set this
	// variant's weight.
	DecisionVersion int
}

// DefaultConfig returns the default A/B suite split by price bracket: code
// author roles use cheap variants, while planning and review use premium ones.
//
// Every experiment enrolls each supported provider at equal weight so each role
// is genuinely runnable on any provider and the evaluation scorecard sees
// balanced samples. This is the deliberate "equal agents" posture: rather than
// hardcoding scorecard-derived weights (which favoured codex on implementation
// and excluded copilot from maintenance), selection is uniform and the
// scorecard informs later reweighting rather than a static bias. Watch the
// per-provider breakdown after rollout — a provider that regresses a role can
// be down-weighted here without code changes.
func DefaultConfig() Config {
	enabled := false
	builtinVersion := CurrentBuiltinVersion
	cheap := modeltier.Models(modeltier.Cheap)
	expensive := modeltier.Models(modeltier.Expensive)
	return Config{
		Enabled:              &enabled,
		MinSamplesPerVariant: 20,
		BuiltinVersion:       &builtinVersion,
		Experiments: []Experiment{
			codeAuthorCheapExperiment(cheap),
			codeAuthorMaintenanceCheapExperiment(cheap),
			fixReviewExpensiveExperiment(expensive),
			reviewExpensiveExperiment(expensive),
			reviewTightenInstructionsExperiment(expensive),
			humanReviewRestructureContextExperiment(),
		},
	}
}

func codeAuthorCheapExperiment(cheap map[string]string) Experiment {
	expEnabled := true
	return Experiment{
		ID:             "code-author-cheap",
		Enabled:        &expEnabled,
		AssignmentUnit: "stage",
		Bracket:        "cheap",
		Roles:          []string{"implementation"},
		Variants: []Variant{
			{ID: "claude-sonnet", Provider: providerid.Claude, Model: "sonnet", Tier: "cheap", Weight: 1},
			{ID: "codex-gpt-5.4", Provider: providerid.Codex, Model: cheap[providerid.Codex], Tier: "cheap", Weight: 1},
			{ID: "copilot-sonnet", Provider: providerid.Copilot, Model: cheap[providerid.Copilot], Tier: "cheap", Weight: 1},
			{ID: "opencode-deepseek-v4-flash", Provider: providerid.OpenCode, Model: cheap[providerid.OpenCode], Tier: "cheap", Weight: 1},
			{
				ID:       "pl-41673aa95495-claude-sonnet",
				Provider: providerid.Claude,
				Model:    "sonnet",
				Tier:     "cheap",
				Version:  "pl-41673aa95495",
				Digest:   digestString(ImplementationTightenInstructionsPL41673AA95495),
				PromptTransform: &PromptTransform{
					Op:   "append",
					Text: ImplementationTightenInstructionsPL41673AA95495,
				},
				Weight: 1,
			},
		},
	}
}

func codeAuthorMaintenanceCheapExperiment(cheap map[string]string) Experiment {
	expEnabled := true
	return Experiment{
		ID:             "code-author-maintenance-cheap",
		Enabled:        &expEnabled,
		AssignmentUnit: "stage",
		Bracket:        "cheap",
		Roles:          []string{"pr-fix", "test-runner"},
		Variants: []Variant{
			{ID: "claude-sonnet", Provider: providerid.Claude, Model: "sonnet", Tier: "cheap", Weight: 1},
			{ID: "codex-gpt-5.4", Provider: providerid.Codex, Model: cheap[providerid.Codex], Tier: "cheap", Weight: 1},
			{ID: "copilot-sonnet", Provider: providerid.Copilot, Model: cheap[providerid.Copilot], Tier: "cheap", Weight: 1},
			{ID: "opencode-deepseek-v4-flash", Provider: providerid.OpenCode, Model: cheap[providerid.OpenCode], Tier: "cheap", Weight: 1},
		},
	}
}

func fixReviewExpensiveExperiment(expensive map[string]string) Experiment {
	expEnabled := true
	return Experiment{
		ID:             "fix-review-expensive",
		Enabled:        &expEnabled,
		AssignmentUnit: "stage",
		Bracket:        "expensive",
		Roles:          []string{"fix-review"},
		Variants: []Variant{
			{ID: "claude-opus", Provider: providerid.Claude, Model: "opus", Tier: "expensive", Weight: 1},
			{ID: "codex-gpt-5.5", Provider: providerid.Codex, Model: expensive[providerid.Codex], Tier: "expensive", Weight: 1},
			{ID: "copilot-gemini-3.1-pro", Provider: providerid.Copilot, Model: expensive[providerid.Copilot], Tier: "expensive", Weight: 1},
			{ID: "opencode-glm-5.2", Provider: providerid.OpenCode, Model: expensive[providerid.OpenCode], Tier: "expensive", Weight: 1},
		},
	}
}

func reviewExpensiveExperiment(expensive map[string]string) Experiment {
	expEnabled := true
	return Experiment{
		ID:             "review-expensive",
		Enabled:        &expEnabled,
		AssignmentUnit: "stage",
		Bracket:        "expensive",
		Roles:          []string{"plan"},
		Variants: []Variant{
			{ID: "claude-opus", Provider: providerid.Claude, Model: "opus", Tier: "expensive", Weight: 1},
			{ID: "codex-gpt-5.5", Provider: providerid.Codex, Model: expensive[providerid.Codex], Tier: "expensive", Weight: 1},
			{ID: "copilot-gemini-3.1-pro", Provider: providerid.Copilot, Model: expensive[providerid.Copilot], Tier: "expensive", Weight: 1},
			{ID: "opencode-glm-5.2", Provider: providerid.OpenCode, Model: expensive[providerid.OpenCode], Tier: "expensive", Weight: 1},
		},
	}
}

func reviewTightenInstructionsExperiment(expensive map[string]string) Experiment {
	expEnabled := true
	return Experiment{
		ID:             "review-tighten-instructions-pl-a2d853b2c1d9",
		Kind:           "compound",
		Enabled:        &expEnabled,
		AssignmentUnit: "stage",
		Bracket:        "expensive",
		Subject:        &Subject{Role: "review"},
		Roles:          []string{"review"},
		Variants: []Variant{
			{ID: "claude-opus", Provider: providerid.Claude, Model: "opus", Tier: "expensive", Weight: 1},
			{ID: "codex-gpt-5.5", Provider: providerid.Codex, Model: expensive[providerid.Codex], Tier: "expensive", Weight: 1},
			{ID: "copilot-gemini-3.1-pro", Provider: providerid.Copilot, Model: expensive[providerid.Copilot], Tier: "expensive", Weight: 1},
			{ID: "opencode-glm-5.2", Provider: providerid.OpenCode, Model: expensive[providerid.OpenCode], Tier: "expensive", Weight: 1},
			{
				ID:       "pl-a2d853b2c1d9-codex-gpt-5.5",
				Provider: providerid.Codex,
				Model:    expensive[providerid.Codex],
				Tier:     "expensive",
				Version:  "pl-a2d853b2c1d9",
				Digest:   digestString(ReviewTightenInstructionsPLA2D853B2C1D9),
				PromptTransform: &PromptTransform{
					Op:   "append",
					Text: ReviewTightenInstructionsPLA2D853B2C1D9,
				},
				Weight: 1,
			},
		},
	}
}

// humanReviewRestructureContextExperiment is Prompt Lab proposal
// pl-50bbd0314913 (candidate intent "restructure-context"), scaffolded from
// fleet evidence that role human-review fails 73% vs 29% overall. The
// challenger variant is enrolled only after `sybra-cli evaluation offline
// gate` allows the exact digest below; changing the prompt text invalidates
// that verdict and requires re-running the offline eval before keeping
// positive weight (see internal/prompteval/testdata/promptlab-human-review-
// restructure-context-*.json for the screened fixture).
func humanReviewRestructureContextExperiment() Experiment {
	expEnabled := true
	return Experiment{
		ID:             "human-review-restructure-context-pl-50bbd0314913",
		Kind:           "prompt",
		Enabled:        &expEnabled,
		AssignmentUnit: "task",
		Subject:        &Subject{Role: "human-review"},
		Roles:          []string{"human-review"},
		Variants: []Variant{
			{ID: "baseline", Provider: providerid.Claude, Model: "claude-haiku-4-5-20251001", Weight: 1},
			{
				ID:       "pl-50bbd0314913",
				Provider: providerid.Claude,
				Model:    "claude-haiku-4-5-20251001",
				Version:  "pl-50bbd0314913",
				Digest:   digestString(HumanReviewRestructureContextPL50BBD0314913),
				PromptTransform: &PromptTransform{
					Op:   "prepend",
					Text: HumanReviewRestructureContextPL50BBD0314913,
				},
				Weight: 1,
			},
		},
	}
}

// Validate checks all configured experiments, including disabled experiments.
func (c Config) Validate() error {
	for i := range c.Experiments {
		if err := validateExperiment(c.Experiments[i], nil); err != nil {
			return err
		}
	}
	return nil
}

// WithoutInvalidExperiments returns a copy with every experiment that would
// fail selection-time validation removed, calling report once per drop.
// Selection validates each experiment as it matches (see selectFromExperiment)
// and propagates the error to the dispatcher, so one malformed experiment
// otherwise wedges every role it targets — the workflow step fails to start an
// agent, indefinitely, with nothing logged at startup.
//
// Dropping at load turns that into a warning plus unrouted (default-provider)
// dispatch, which is recoverable. It matters for edits made outside Sybra and
// for configs a code change retroactively invalidates: the per-role
// reasoning-effort baseline decides what an omitted variant effort resolves to,
// so retuning a role can invalidate an operator's prompt/skill experiment.
func (c Config) WithoutInvalidExperiments(report func(id string, err error)) Config {
	if len(c.Experiments) == 0 {
		return c
	}
	kept := make([]Experiment, 0, len(c.Experiments))
	for i := range c.Experiments {
		exp := c.Experiments[i]
		if err := validateExperiment(exp, nil); err != nil {
			if report != nil {
				report(exp.ID, err)
			}
			continue
		}
		kept = append(kept, exp)
	}
	c.Experiments = kept
	return c
}

// EnabledValue reports whether A/B assignment should run.
func (c Config) EnabledValue() bool {
	return c.Enabled != nil && *c.Enabled
}

func (c Config) BuiltinVersionValue() int {
	if c.BuiltinVersion == nil {
		return 0
	}
	return *c.BuiltinVersion
}

func (e Experiment) EnabledValue() bool {
	return e.Enabled == nil || *e.Enabled
}

func (e Experiment) KindValue() string {
	kind := strings.TrimSpace(e.Kind)
	if kind == "" {
		return "model"
	}
	return kind
}
