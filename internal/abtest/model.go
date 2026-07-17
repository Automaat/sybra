package abtest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Automaat/sybra/internal/modeltier"
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
}

// CurrentBuiltinVersion is the version stamp for the built-in experiment set
// returned by DefaultConfig. Bump this whenever the built-in experiments'
// roles, variants, or weights change in a way that persisted configs should
// pick up automatically.
const CurrentBuiltinVersion = 7

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

If steps 1-2 already establish a clear stall reason — a genuine human-required cause, or a Sybra bug pattern — do not let unrelated noise later in the log tail override that conclusion. In "summary", name which of steps 1-2 grounded your decision before citing any log excerpt.
`

const ReviewTightenInstructionsPLA2D853B2C1D9 = `

Review variant pl-a2d853b2c1d9: apply a stricter staff-code-review standard before returning a verdict.

- Lead with concrete blocking defects only: correctness, security, data loss, broken workflows, test gaps that can hide a regression, or repository-rule violations.
- For every finding, name the exact file/line evidence and the user-visible failure mode. If you cannot tie a concern to behavior or a repo rule, omit it.
- Check that implementation and tests cover the task's current acceptance criteria, including edge cases and failure paths. Treat missing focused tests as a finding when the change is risky or user-visible.
- Separate must-fix-before-merge issues from optional cleanup; do not block on style preferences, speculative rewrites, or unrelated refactors.
- If no blocking issues remain, say that clearly and call out any residual test or runtime verification gap.
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
	Provider        string
	Model           string
	ReasoningEffort string
	AssignmentUnit  string
	AssignmentKey   string
	PromptTransform *PromptTransform
	SkillAliases    map[string]string
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
	enabled := true
	expEnabled := true
	builtinVersion := CurrentBuiltinVersion
	cheap := modeltier.Models(modeltier.Cheap)
	expensive := modeltier.Models(modeltier.Expensive)
	return Config{
		Enabled:              &enabled,
		MinSamplesPerVariant: 20,
		BuiltinVersion:       &builtinVersion,
		Experiments: []Experiment{
			{
				ID:             "code-author-cheap",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "cheap",
				Roles:          []string{"implementation"},
				Variants: []Variant{
					{ID: "claude-sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 1},
					{ID: "codex-gpt-5.4", Provider: "codex", Model: cheap["codex"], Tier: "cheap", Weight: 1},
					{ID: "copilot-sonnet", Provider: "copilot", Model: cheap["copilot"], Tier: "cheap", Weight: 1},
					{ID: "opencode-deepseek-v4-flash", Provider: "opencode", Model: cheap["opencode"], Tier: "cheap", Weight: 1},
				},
			},
			{
				ID:             "code-author-maintenance-cheap",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "cheap",
				Roles:          []string{"pr-fix", "test-runner"},
				Variants: []Variant{
					{ID: "claude-sonnet", Provider: "claude", Model: "sonnet", Tier: "cheap", Weight: 1},
					{ID: "codex-gpt-5.4", Provider: "codex", Model: cheap["codex"], Tier: "cheap", Weight: 1},
					{ID: "copilot-sonnet", Provider: "copilot", Model: cheap["copilot"], Tier: "cheap", Weight: 1},
					{ID: "opencode-deepseek-v4-flash", Provider: "opencode", Model: cheap["opencode"], Tier: "cheap", Weight: 1},
				},
			},
			{
				ID:             "fix-review-expensive",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "expensive",
				Roles:          []string{"fix-review"},
				Variants: []Variant{
					{ID: "claude-opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
					{ID: "codex-gpt-5.5", Provider: "codex", Model: expensive["codex"], Tier: "expensive", Weight: 1},
					{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: expensive["copilot"], Tier: "expensive", Weight: 1},
					{ID: "opencode-glm-5.2", Provider: "opencode", Model: expensive["opencode"], Tier: "expensive", Weight: 1},
				},
			},
			{
				ID:             "review-expensive",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "expensive",
				Roles:          []string{"plan"},
				Variants: []Variant{
					{ID: "claude-opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
					{ID: "codex-gpt-5.5", Provider: "codex", Model: expensive["codex"], Tier: "expensive", Weight: 1},
					{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: expensive["copilot"], Tier: "expensive", Weight: 1},
					{ID: "opencode-glm-5.2", Provider: "opencode", Model: expensive["opencode"], Tier: "expensive", Weight: 1},
				},
			},
			{
				ID:             "review-tighten-instructions-pl-a2d853b2c1d9",
				Kind:           "compound",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "expensive",
				Subject:        &Subject{Role: "review"},
				Roles:          []string{"review"},
				Variants: []Variant{
					{ID: "claude-opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
					{ID: "codex-gpt-5.5", Provider: "codex", Model: expensive["codex"], Tier: "expensive", Weight: 1},
					{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: expensive["copilot"], Tier: "expensive", Weight: 1},
					{ID: "opencode-glm-5.2", Provider: "opencode", Model: expensive["opencode"], Tier: "expensive", Weight: 1},
					{
						ID:       "pl-a2d853b2c1d9-codex-gpt-5.5",
						Provider: "codex",
						Model:    expensive["codex"],
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
			},
			humanReviewRestructureContextExperiment(expEnabled),
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
func humanReviewRestructureContextExperiment(expEnabled bool) Experiment {
	return Experiment{
		ID:             "human-review-restructure-context-pl-50bbd0314913",
		Kind:           "prompt",
		Enabled:        &expEnabled,
		AssignmentUnit: "task",
		Subject:        &Subject{Role: "human-review"},
		Roles:          []string{"human-review"},
		Variants: []Variant{
			{ID: "baseline", Provider: "claude", Model: "claude-haiku-4-5-20251001", Weight: 1},
			{
				ID:       "pl-50bbd0314913",
				Provider: "claude",
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

// EnabledValue reports whether A/B assignment should run.
func (c Config) EnabledValue() bool {
	return c.Enabled == nil || *c.Enabled
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
