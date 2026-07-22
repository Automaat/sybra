package runacct

import (
	"time"

	"github.com/Automaat/sybra/internal/runoutcome"
)

type Record struct {
	ID                 string
	TaskID             string
	Role               string
	Mode               string
	Provider           string
	Model              string
	ReasoningEffort    string
	ExperimentID       string
	VariantID          string
	SkillExecutionMode string
	SkillConformance   string
	CostUSD            float64
	DurationS          float64
	PremiumRequests    float64
	TurnCount          int
	ToolCalls          int
	Outcome            string
	Timestamp          time.Time
}

type Counts struct {
	Runs     int
	Failures int
	Stalled  int
	Resolved int
	Unknown  int
}

type CountConfig struct {
	CountsTowardFailure func(Record) bool
}

func Count(records []Record, match func(Record) bool, cfg CountConfig) Counts {
	var c Counts
	for i := range records {
		r := records[i]
		if match != nil && !match(r) {
			continue
		}
		c.Runs++
		outcome := runoutcome.Normalize(r.Outcome)
		switch {
		case runoutcome.IsStallLike(outcome):
			c.Stalled++
		case runoutcome.IsTerminal(outcome):
			if cfg.CountsTowardFailure == nil || cfg.CountsTowardFailure(r) {
				c.Resolved++
				if outcome == runoutcome.Failed {
					c.Failures++
				}
			}
		default:
			c.Unknown++
		}
	}
	return c
}

func CountsTowardCodeAuthorFailureRate(r Record) bool {
	return IsCodeAuthorRole(r.Role)
}

func IsCodeAuthorRole(role string) bool {
	switch NormalizedRole(role) {
	case "implementation", "fix-review", "pr-fix", "test-fix":
		return true
	default:
		return false
	}
}

func NormalizedRole(role string) string {
	if role == "" {
		return "implementation"
	}
	return role
}
