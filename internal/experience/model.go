package experience

import "time"

// Record is a compact, verified memory item distilled from a successfully
// landed task. Stores are scrub-agnostic; callers must scrub work-derived
// fields before Put.
type Record struct {
	TaskID         string    `json:"task_id"`
	CreatedAt      time.Time `json:"created_at"`
	ProjectID      string    `json:"project_id"`
	ProjectType    string    `json:"project_type"`
	Title          string    `json:"title"`
	Tags           []string  `json:"tags,omitempty"`
	Size           string    `json:"size,omitempty"`
	Type           string    `json:"type,omitempty"`
	AgentMode      string    `json:"agent_mode,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Outcome        string    `json:"outcome,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
	FailureModes   []string  `json:"failure_modes,omitempty"`
	Strategy       string    `json:"strategy,omitempty"`
	VerifyCommands []string  `json:"verify_commands,omitempty"`
	Caution        string    `json:"caution,omitempty"`
}
