package agent

// ErrorEvent is the payload emitted on agent:error:{id}.
type ErrorEvent struct {
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// PluginErrorsEvent is emitted on agent:plugin_errors:{id} when the init event
// carries plugin load failures.
type PluginErrorsEvent struct {
	Errors []string `json:"errors"`
}

// EscalationEvent is emitted on agent:escalation:{id} when a guardrail fires.
type EscalationEvent struct {
	// Reason is "turns" or "cost".
	Reason    string  `json:"reason"`
	TurnCount int     `json:"turnCount,omitempty"`
	CostUSD   float64 `json:"costUsd,omitempty"`
	Limit     float64 `json:"limit"`
}
