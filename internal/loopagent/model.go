package loopagent

import (
	"fmt"
	"strings"
	"time"
)

// minIntervalSec is the lower bound for a loop's tick. Anything tighter than
// a minute either burns cost or thunder-herds the agent manager.
const minIntervalSec = 60

// LoopAgent is a recurring headless agent: every IntervalSec seconds the
// scheduler spawns a fresh `claude -p Prompt` process. Records are persisted
// one-per-file under ~/.synapse/loop-agents/<id>.yaml.
type LoopAgent struct {
	ID           string    `yaml:"id" json:"id"`
	Name         string    `yaml:"name" json:"name"`
	Prompt       string    `yaml:"prompt" json:"prompt"`
	IntervalSec  int       `yaml:"interval_sec" json:"intervalSec"`
	AllowedTools []string  `yaml:"allowed_tools" json:"allowedTools"`
	Provider     string    `yaml:"provider" json:"provider"`
	Model        string    `yaml:"model" json:"model"`
	Enabled      bool      `yaml:"enabled" json:"enabled"`
	LastRunAt    time.Time `yaml:"last_run_at,omitempty" json:"lastRunAt"`
	LastRunID    string    `yaml:"last_run_id,omitempty" json:"lastRunId"`
	LastRunCost  float64   `yaml:"last_run_cost,omitempty" json:"lastRunCost"`
	CreatedAt    time.Time `yaml:"created_at" json:"createdAt"`
	UpdatedAt    time.Time `yaml:"updated_at" json:"updatedAt"`
}

// Validate enforces invariants the store and scheduler rely on.
func (la *LoopAgent) Validate() error {
	if strings.TrimSpace(la.Name) == "" {
		return fmt.Errorf("loop agent name is required")
	}
	if strings.TrimSpace(la.Prompt) == "" {
		return fmt.Errorf("loop agent prompt is required")
	}
	if la.IntervalSec < minIntervalSec {
		return fmt.Errorf("loop agent interval_sec must be >= %d (got %d)", minIntervalSec, la.IntervalSec)
	}
	if la.Provider != "" && la.Provider != "claude" {
		return fmt.Errorf("loop agent provider must be claude (got %q)", la.Provider)
	}
	return nil
}

// AgentName is the Name applied to spawned agents so the scheduler and the
// audit log can identify a run as belonging to this loop.
func (la *LoopAgent) AgentName() string {
	return "loop:" + la.Name
}
