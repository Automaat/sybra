package llmjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/llmexec"
	"github.com/Automaat/sybra/internal/provider"
)

const defaultMaxRepairs = 2

type runnerFunc func(context.Context, string, llmexec.Options) (llmexec.Result, error)

var runJSON runnerFunc = llmexec.RunJSON

// Spec describes a short structured-output LLM job.
type Spec[T any] struct {
	Name          string
	Tier          Tier
	Schema        string
	Validate      func(*T) error
	MaxRepairs    int
	AvoidProvider string
}

type Meta struct {
	Provider string
	Tier     Tier
	Repairs  int
}

// Run invokes a short JSON job, optionally repairing malformed or invalid
// responses with fresh one-shot attempts.
func Run[T any](ctx context.Context, prompt string, s Spec[T], o llmexec.Options) (T, Meta, error) {
	var zero T
	o.Models = modelsFor(s.Tier)
	if strings.TrimSpace(s.Schema) != "" {
		prompt += "\n\nOutput schema:\n" + strings.TrimSpace(s.Schema)
	}
	if strings.TrimSpace(s.AvoidProvider) != "" {
		var err error
		o, err = avoidProvider(o, s.AvoidProvider)
		if err != nil {
			logJob(o.Logger, s.Name, "", s.Tier, 0, false)
			return zero, Meta{Tier: s.Tier}, err
		}
	}

	maxRepairs := s.MaxRepairs
	if maxRepairs == 0 {
		maxRepairs = defaultMaxRepairs
	}
	attempts := 1 + maxRepairs
	attemptPrompt := prompt
	var lastErr error
	var lastProvider string
	for attempt := range attempts {
		res, err := runJSON(ctx, attemptPrompt, o)
		if err != nil {
			lastErr = err
			lastProvider = res.Provider
			break
		}
		lastProvider = res.Provider
		var out T
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			lastErr = fmt.Errorf("parse JSON: %w", err)
		} else if s.Validate != nil {
			lastErr = s.Validate(&out)
		} else {
			lastErr = nil
		}
		if lastErr == nil {
			repairs := attempt
			logJob(o.Logger, s.Name, res.Provider, s.Tier, repairs, true)
			return out, Meta{Provider: res.Provider, Tier: s.Tier, Repairs: repairs}, nil
		}
		if attempt < attempts-1 {
			attemptPrompt = prompt + "\n\nYour previous output was invalid: " + lastErr.Error() + ". Re-emit ONLY the corrected JSON object."
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no result")
	}
	logJob(o.Logger, s.Name, lastProvider, s.Tier, maxRepairs, false)
	return zero, Meta{Provider: lastProvider, Tier: s.Tier, Repairs: maxRepairs},
		fmt.Errorf("%s job failed with provider %q after %d attempts: %w", s.Name, lastProvider, attempts, lastErr)
}

func avoidProvider(o llmexec.Options, avoid string) (llmexec.Options, error) {
	avoid = strings.ToLower(strings.TrimSpace(avoid))
	if avoid == "" {
		return o, nil
	}
	preferred := strings.ToLower(strings.TrimSpace(o.Provider))
	if preferred != "" && preferred != avoid {
		o.Gate = avoidingGate{base: o.Gate, avoid: avoid}
		return o, nil
	}
	for _, p := range []string{"claude", "codex", "copilot"} {
		if p != avoid {
			o.Provider = p
			o.Gate = avoidingGate{base: o.Gate, avoid: avoid}
			return o, nil
		}
	}
	return o, fmt.Errorf("no provider remains after avoiding %q", avoid)
}

type avoidingGate struct {
	base  provider.HealthGate
	avoid string
}

func (g avoidingGate) IsHealthy(providerName string) bool {
	if providerName == g.avoid {
		return false
	}
	if g.base == nil {
		return true
	}
	return g.base.IsHealthy(providerName)
}

func (g avoidingGate) Reason(providerName string) string {
	if providerName == g.avoid {
		return "avoided for independence"
	}
	if g.base == nil {
		return ""
	}
	return g.base.Reason(providerName)
}

func (g avoidingGate) RateLimited(providerName string) bool {
	if providerName == g.avoid {
		return false
	}
	return g.base != nil && g.base.RateLimited(providerName)
}

func (g avoidingGate) Failover(unhealthy string) string {
	if g.base == nil {
		return ""
	}
	next := g.base.Failover(unhealthy)
	if next == g.avoid {
		return ""
	}
	return next
}

func (g avoidingGate) ReportAuthFailure(providerName, reason string) {
	if g.base != nil && providerName != g.avoid {
		g.base.ReportAuthFailure(providerName, reason)
	}
}

func (g avoidingGate) ReportRateLimit(providerName string, retryAfter time.Duration, reason string) {
	if g.base != nil && providerName != g.avoid {
		g.base.ReportRateLimit(providerName, retryAfter, reason)
	}
}

func logJob(logger *slog.Logger, name, providerName string, tier Tier, repairs int, ok bool) {
	if logger == nil {
		return
	}
	logger.Info("llmjob.done", "name", name, "provider", providerName, "tier", tier, "repairs", repairs, "ok", ok)
}
