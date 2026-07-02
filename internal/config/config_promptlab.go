package config

// PromptLabConfig controls the automated Prompt Lab loop that scaffolds
// versioned prompt/skill variant proposals from fleet evidence (Evaluation
// report + stats). It never authors or applies prompt/skill text itself —
// every proposal is filed as a reviewed local task for a human (or a later
// agent run) to author and gate through the standard offline-eval + A/B
// path. Disabled by default: unlike HarnessEvolveConfig, a fresh install
// must not start filing tasks before an operator opts in.
type PromptLabConfig struct {
	Enabled            bool    `yaml:"enabled" json:"enabled"`
	IntervalHours      float64 `yaml:"interval_hours" json:"intervalHours"`
	LookbackHours      float64 `yaml:"lookback_hours" json:"lookbackHours"`
	MinSamples         int     `yaml:"min_samples" json:"minSamples"`
	MinEffectSize      float64 `yaml:"min_effect_size" json:"minEffectSize"`
	MaxProposalsPerRun int     `yaml:"max_proposals_per_run" json:"maxProposalsPerRun"`
}
