package config

// PromptLabConfig controls the automated Prompt Lab loop that scaffolds
// versioned prompt/skill variant proposals from fleet evidence (Evaluation
// report + stats). It never authors or applies prompt/skill text itself —
// every proposal is filed as a local task whose authoring workflow gates the
// resulting variant through the standard offline-eval + A/B path. Disabled
// by default: unlike HarnessEvolveConfig, a fresh install must not start
// filing tasks before an operator opts in.
//
// AutoApprove starts a filed proposal's authoring workflow without waiting
// for a human click, making the loop autonomous end to end. It defaults to
// true (see Config.PromptLabAutoApprove): a proposal only ever scaffolds an
// *attempt*, and the barrier that actually protects production is
// prompteval.Gate.AllowEnrollment inside the authoring workflow, which is
// fail-closed and unaffected by this setting. A proposal carrying an
// explicit FAILED offline verdict is never auto-approved regardless.
type PromptLabConfig struct {
	Enabled            bool    `yaml:"enabled" json:"enabled"`
	IntervalHours      float64 `yaml:"interval_hours" json:"intervalHours"`
	LookbackHours      float64 `yaml:"lookback_hours" json:"lookbackHours"`
	MinSamples         int     `yaml:"min_samples" json:"minSamples"`
	MinEffectSize      float64 `yaml:"min_effect_size" json:"minEffectSize"`
	MaxProposalsPerRun int     `yaml:"max_proposals_per_run" json:"maxProposalsPerRun"`
	AutoApprove        *bool   `yaml:"auto_approve" json:"autoApprove"`
}
