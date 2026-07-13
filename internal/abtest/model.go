package abtest

import "strings"

// ApplyPromptTransform rewrites prompt per an assigned variant's transform:
// "replace"/"template" swaps in Text outright, "prepend"/"append" splice
// Text around the original prompt, and a nil transform (or unrecognized Op)
// leaves prompt unchanged. Shared by every non-workflow dispatch site that
// routes through Manager.ApplyABVariant (human-review, orchestrator, staff PR
// review) so a "prompt" experiment's variant text actually reaches the model
// instead of only stamping provider/model attribution.
func ApplyPromptTransform(prompt string, t *PromptTransform) string {
	if t == nil {
		return prompt
	}
	switch strings.TrimSpace(t.Op) {
	case "replace", "template":
		return t.Text
	case "prepend":
		return t.Text + prompt
	case "append":
		return prompt + t.Text
	default:
		return prompt
	}
}

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
const CurrentBuiltinVersion = 5

// BuiltinExperimentIDs lists the experiment IDs owned by Sybra's shipped
// defaults. A persisted config's experiment is only replaced during a builtin
// reconcile if its ID appears here — every other experiment ID is treated as
// user-authored and preserved verbatim.
var BuiltinExperimentIDs = []string{"code-author-cheap", "code-author-maintenance-cheap", "fix-review-expensive", "review-expensive", "human-review-pl-5cf660095cb8"}

// humanReviewDedupeAddendum is Prompt Lab proposal pl-5cf660095cb8's
// candidate text for the human-review role: appended to the base
// human-review prompt (internal/sybra buildPrompt), it tells the reviewer to
// defer to an existing correct diagnosis already recorded on the task instead
// of re-investigating and re-filing. Its sha256 digest (see the
// pl-5cf660095cb8 variant below) must match the exact bytes screened by
// `sybra-cli evaluation offline run` — editing this text invalidates that
// verdict and re-blocks enrollment until the offline eval is re-run.
const humanReviewDedupeAddendum = `
## Before filing (dedupe check)

Before returning 'sybra_bug', or diagnosing at all: scan the task body for
existing '## Auto-review verdict' sections and the progress log for prior
'decision'/'blocker'/'failure' entries. If one of them already correctly
explains the CURRENT status and status_reason and no new evidence has
appeared since it was written, do not re-run the investigation or file a
second issue for the same root cause — return the SAME decision as that
entry, restate its diagnosis in 'summary', and leave 'issue_title',
'issue_body', and 'issue_labels' null so you never duplicate a filing that
already exists.
`

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
// Every experiment enrolls all three providers at equal weight (1:1:1) so each
// role is genuinely runnable on any provider and the evaluation scorecard sees
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
					{ID: "codex-gpt-5.4", Provider: "codex", Model: "gpt-5.4", Tier: "cheap", Weight: 1},
					{ID: "copilot-sonnet", Provider: "copilot", Model: "claude-sonnet-4.6", Tier: "cheap", Weight: 1},
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
					{ID: "codex-gpt-5.4", Provider: "codex", Model: "gpt-5.4", Tier: "cheap", Weight: 1},
					{ID: "copilot-sonnet", Provider: "copilot", Model: "claude-sonnet-4.6", Tier: "cheap", Weight: 1},
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
					{ID: "codex-gpt-5.5", Provider: "codex", Model: "gpt-5.5", Tier: "expensive", Weight: 1},
					{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: "gemini-3.1-pro-preview", Tier: "expensive", Weight: 1},
				},
			},
			{
				ID:             "review-expensive",
				Enabled:        &expEnabled,
				AssignmentUnit: "stage",
				Bracket:        "expensive",
				Roles:          []string{"review", "plan"},
				Variants: []Variant{
					{ID: "claude-opus", Provider: "claude", Model: "opus", Tier: "expensive", Weight: 1},
					{ID: "codex-gpt-5.5", Provider: "codex", Model: "gpt-5.5", Tier: "expensive", Weight: 1},
					{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: "gemini-3.1-pro-preview", Tier: "expensive", Weight: 1},
				},
			},
			// human-review-pl-5cf660095cb8: Prompt Lab proposal
			// pl-5cf660095cb8 (candidate intent "tighten-instructions"),
			// scaffolded from fleet evidence that role human-review fails
			// 19% vs 11% overall. The challenger variant's weight stays 0
			// (never selected — see abtest.EligibleVariants) until
			// `sybra-cli evaluation offline gate` allows its digest to
			// enroll; bumping the weight before that is exactly the
			// unevaluated-enrollment this gate exists to prevent.
			{
				ID:             "human-review-pl-5cf660095cb8",
				Kind:           "prompt",
				Enabled:        &expEnabled,
				AssignmentUnit: "task",
				Subject:        &Subject{Role: "human-review"},
				Roles:          []string{"human-review"},
				Variants: []Variant{
					{ID: "baseline", Provider: "claude", Model: "claude-haiku-4-5-20251001", Weight: 1},
					{
						ID:       "pl-5cf660095cb8",
						Provider: "claude",
						Model:    "claude-haiku-4-5-20251001",
						Digest:   "e0e4bf727cb35b08b97197c4a849445162ac82a834f760fa996731692f4d9c86",
						PromptTransform: &PromptTransform{
							Op:   "append",
							Text: humanReviewDedupeAddendum,
						},
						Weight: 0,
					},
				},
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
