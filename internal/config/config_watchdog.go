package config

// WatchdogConfig controls the in-process agent watchdog (internal/watchdog),
// which supervises running headless agents: it triggers a cheap LLM inspection
// when an agent stalls, overruns its size budget, or loops on the same tool
// call (real-time loop detection), then stops/escalates/nudges based on the
// verdict. Enabled defaults to true — the watchdog is an always-on safety net,
// not an opt-in automation. Model selects the cheap judge model; LoopThreshold
// is the number of consecutive identical tool-call signatures that flags a loop.
// MaxRunsPerWindow and RunWindowMinutes bound how many agent runs of the same
// role a single task may accumulate within a trailing window before being
// escalated to human-required — a rate-based backstop for a redispatch loop
// that the dwell budget (hours) would only catch much later.
type WatchdogConfig struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Model            string `yaml:"model" json:"model"`
	LoopThreshold    int    `yaml:"loop_threshold" json:"loopThreshold"`
	MaxRunsPerWindow int    `yaml:"max_runs_per_window" json:"maxRunsPerWindow"`
	RunWindowMinutes int    `yaml:"run_window_minutes" json:"runWindowMinutes"`
}
