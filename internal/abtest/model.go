package abtest

import "strings"

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
const CurrentBuiltinVersion = 3

// BuiltinExperimentIDs lists the experiment IDs owned by Sybra's shipped
// defaults. A persisted config's experiment is only replaced during a builtin
// reconcile if its ID appears here — every other experiment ID is treated as
// user-authored and preserved verbatim.
var BuiltinExperimentIDs = []string{"code-author-cheap", "code-author-maintenance-cheap", "fix-review-expensive", "review-expensive"}

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
// author roles use cheap variants, while planning and review use premium
// ones.
//
// The cheap bracket is split into two role-scoped experiments rather than one
// shared bracket. The live evaluation scorecard (30d, 2026-07-05) grounds the
// weights:
//   - implementation: claude:sonnet:medium is the highest-quality/highest-cost
//     option (failureRate 0.059, mergeRate 0.933, ~$2.58/run over 236 runs);
//     codex:gpt-5.5 is comparable-to-better at a fraction of the cost
//     (failureRate 0.038-0.13, mergeRate 1.0, ~$0.31/run); copilot:gpt-5.5
//     lands respectably too (failureRate 0.021, mergeRate 0.833, ~$0.07/run).
//     Weighted 1:2:1 (claude:codex:copilot) — codex takes the plurality,
//     copilot gets a capped (not blanket) exposure, claude keeps a non-zero
//     exploration/fallback floor.
//   - fix-review/pr-fix/test-runner ("maintenance"): codex clearly wins on
//     pr-fix (failureRate 0.036 vs claude's 0.084) and test-runner (0.037 vs
//     claude's 0.268), and is only modestly behind claude on fix-review
//     (0.095 vs 0.014). Copilot's maintenance-role sample is too thin to
//     enable here (see decision log) — weighted 1:3 (claude:codex), no
//     copilot, until more data justifies widening it.
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
					{ID: "codex-gpt-5.4", Provider: "codex", Model: "gpt-5.4", Tier: "cheap", Weight: 2},
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
					{ID: "codex-gpt-5.4", Provider: "codex", Model: "gpt-5.4", Tier: "cheap", Weight: 3},
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
