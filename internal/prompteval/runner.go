package prompteval

import (
	"context"

	"github.com/Automaat/sybra/internal/config"
)

// OfflineRunner executes a Spec against a real model/CLI and returns the raw
// Result. Run returns an error (never a zero-value Result) when the run
// itself could not be measured, so a caller can map that to Status
// unavailable rather than a silent pass.
type OfflineRunner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
	Available() bool
	Name() string
}

// SelectRunner resolves the configured offline runner. "promptfoo" and
// "native" pin a specific runner; "auto" (and empty) prefers promptfoo when
// its binary is on PATH, falling back to native so the gate still works on a
// mise-only server where Node/promptfoo is absent.
func SelectRunner(cfg config.OfflineEvalConfig) OfflineRunner {
	promptfoo := NewPromptfooRunner(cfg.BinaryPath)
	switch cfg.Runner {
	case "promptfoo":
		return promptfoo
	case "native":
		return NewNativeRunner()
	default:
		if promptfoo.Available() {
			return promptfoo
		}
		return NewNativeRunner()
	}
}
