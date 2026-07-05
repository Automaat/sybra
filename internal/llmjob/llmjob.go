package llmjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// AttemptTimeout, when set, bounds a single runJSON invocation instead of
	// letting the caller's ctx govern the whole call directly. Each attempt
	// (including repairs) gets its own context.WithTimeout derived fresh from
	// ctx, so one slow/hung attempt cannot consume the budget a later retry
	// needs. It also opts the job into retrying after a hard runJSON error
	// (not just a malformed/invalid response) — with a shared, un-bounded
	// per-attempt budget that retry would just re-hit the same expired
	// deadline, so it stays gated on AttemptTimeout being set. Zero leaves
	// legacy behavior unchanged: the whole call shares ctx, and a runJSON
	// error is fatal (no retry).
	AttemptTimeout time.Duration
}

// Attempts returns the total number of runJSON invocations Run will make for
// this Spec (the initial attempt plus repairs), so callers that need to size
// an outer deadline around Run don't have to hand-maintain their own copy of
// 1+maxRepairs.
func (s Spec[T]) Attempts() int {
	maxRepairs := s.MaxRepairs
	if maxRepairs == 0 {
		maxRepairs = defaultMaxRepairs
	}
	return 1 + maxRepairs
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
	o.Models = mergeModels(modelsFor(s.Tier), o.Models)
	o.Schema = s.Schema
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
	attempts := s.Attempts()
	attemptPrompt := prompt
	var lastErr error
	var lastProvider string
	madeAttempts := 0
	for attempt := range attempts {
		madeAttempts = attempt + 1
		res, err := runAttempt(ctx, s.AttemptTimeout, attemptPrompt, o)
		if err != nil {
			lastErr = err
			lastProvider = res.Provider
			if s.AttemptTimeout > 0 && attempt < attempts-1 && ctx.Err() == nil {
				continue
			}
			break
		}
		lastProvider = res.Provider
		var out T
		jsonText := extractLastJSONObject(res.Text)
		if jsonText == "" {
			lastErr = fmt.Errorf("parse JSON: no JSON object in result: %q", res.Text)
		} else if err := json.Unmarshal([]byte(jsonText), &out); err != nil {
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
		fmt.Errorf("%s job failed with provider %q after %d attempts: %w", s.Name, lastProvider, madeAttempts, lastErr)
}

// runAttempt runs one runJSON invocation, optionally bounding it with its own
// fresh timeout derived from ctx rather than letting a shared ambient
// deadline decide how much of the budget is left by the time this attempt
// starts.
func runAttempt(ctx context.Context, timeout time.Duration, prompt string, o llmexec.Options) (llmexec.Result, error) {
	if timeout <= 0 {
		return runJSON(ctx, prompt, o)
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runJSON(actx, prompt, o)
}

func mergeModels(defaults, explicit map[string]string) map[string]string {
	out := make(map[string]string, len(defaults)+len(explicit))
	maps.Copy(out, defaults)
	maps.Copy(out, explicit)
	return out
}

// extractLastJSONObject returns the last balanced {...} substring in s, or "".
// It tolerates common provider wrappers such as prose and fenced code blocks.
func extractLastJSONObject(s string) string {
	s = strings.TrimSpace(s)
	var (
		inString  bool
		escape    bool
		depth     int
		objStart  = -1
		lastStart = -1
		lastEnd   = -1
	)
	for i := range len(s) {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			if depth == 0 {
				objStart = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && objStart >= 0 {
				lastStart = objStart
				lastEnd = i
				objStart = -1
			}
		}
	}
	if lastStart < 0 {
		return ""
	}
	return s[lastStart : lastEnd+1]
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
