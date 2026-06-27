package abtest

// Config controls deterministic A/B assignment for workflow agent runs.
type Config struct {
	Enabled              *bool        `yaml:"enabled" json:"enabled"`
	MinSamplesPerVariant int          `yaml:"min_samples_per_variant" json:"minSamplesPerVariant"`
	Experiments          []Experiment `yaml:"experiments" json:"experiments"`
}

// Experiment selects among variants for matching workflow roles.
type Experiment struct {
	ID             string    `yaml:"id" json:"id"`
	Enabled        *bool     `yaml:"enabled" json:"enabled"`
	AssignmentUnit string    `yaml:"assignment_unit" json:"assignmentUnit"` // "task" or "stage"
	Roles          []string  `yaml:"roles" json:"roles"`
	Variants       []Variant `yaml:"variants" json:"variants"`
}

// Variant is one provider/model arm of an experiment.
type Variant struct {
	ID              string `yaml:"id" json:"id"`
	Provider        string `yaml:"provider" json:"provider"`
	Model           string `yaml:"model" json:"model"`
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoningEffort,omitempty"`
	Weight          int    `yaml:"weight" json:"weight"`
}

// Assignment is the durable identity selected for a workflow stage.
type Assignment struct {
	ExperimentID    string
	VariantID       string
	Provider        string
	Model           string
	ReasoningEffort string
	AssignmentUnit  string
	AssignmentKey   string
}

// DefaultConfig returns the default A/B suite: Claude Code Opus, Codex GPT-5.5,
// and Copilot across Opus, GPT-5.5, and the current best Gemini model.
func DefaultConfig() Config {
	enabled := true
	expEnabled := true
	return Config{
		Enabled:              &enabled,
		MinSamplesPerVariant: 20,
		Experiments: []Experiment{{
			ID:             "code-author-models-v1",
			Enabled:        &expEnabled,
			AssignmentUnit: "stage",
			Roles:          []string{"implementation", "fix-review", "pr-fix"},
			Variants: []Variant{
				{ID: "claude-opus", Provider: "claude", Model: "opus", Weight: 1},
				{ID: "codex-gpt-5.5", Provider: "codex", Model: "gpt-5.5", Weight: 1},
				{ID: "copilot-opus", Provider: "copilot", Model: "claude-opus-4.6", Weight: 1},
				{ID: "copilot-gpt-5.5", Provider: "copilot", Model: "gpt-5.5", Weight: 1},
				{ID: "copilot-gemini-3.1-pro", Provider: "copilot", Model: "gemini-3.1-pro-preview", Weight: 1},
			},
		}},
	}
}

// EnabledValue reports whether A/B assignment should run.
func (c Config) EnabledValue() bool {
	return c.Enabled == nil || *c.Enabled
}

func (e Experiment) EnabledValue() bool {
	return e.Enabled == nil || *e.Enabled
}
