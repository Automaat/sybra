package agent

import (
	"fmt"
	"strings"

	providerpkg "github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/providerid"
)

type headlessInvocation struct {
	name    string
	args    []string
	env     []string
	command string
}

type Provider interface {
	Name() string
	NormalizeModel(model string) string
	BuildCommand(cfg RunConfig, model string) string
	BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error)
	ParseHeadlessLine(line []byte) (StreamEvent, error)
	SandboxArgs(requirePerms, headless bool) []string
	OutputSchemaAsFile() bool
	// EnforcesOutputSchema reports whether this provider actually passes
	// RunConfig.OutputSchema to the spawned CLI so the model is forced to emit
	// schema-valid JSON. Only such providers make a trailing conformance
	// receipt unsatisfiable — copilot/opencode silently ignore the schema, so a
	// receipt must still be appended and verified for them.
	EnforcesOutputSchema() bool
	SessionFilePath(sessionID string) string
	ClassifyError(sample providerpkg.ErrorSample) providerpkg.Classification
	// HonorsAllowedTools reports whether this provider actually enforces
	// RunConfig.AllowedTools on the spawned CLI. False means the list is
	// silently ignored and the agent runs with the provider's own default
	// reach — see warnUnenforceableAllowedTools.
	HonorsAllowedTools() bool
	// SupportsOutputSchema reports whether this provider actually applies
	// RunConfig.OutputSchema to the spawned CLI. False means the schema is
	// silently ignored and the run falls back to whatever plain-text contract
	// the step's own prompt anticipates — see resolveWorkflowSkillPrompt's
	// schemaEnforced check.
	SupportsOutputSchema() bool
}

type baseProvider struct{}

func (baseProvider) SandboxArgs(bool, bool) []string { return nil }

// HonorsAllowedTools defaults to false so a provider only claims to enforce the
// list by saying so. codex has no per-tool allowlist at all (only --sandbox,
// which is filesystem-level), and copilot's --allow-tool vocabulary is
// unrelated to claude's tool names — a guessed mapping would manufacture a
// boundary that does not hold, which is worse than an honest gap.
func (baseProvider) HonorsAllowedTools() bool { return false }

// SupportsOutputSchema defaults to false for the same reason
// HonorsAllowedTools does — a provider only claims to enforce the schema by
// saying so, rather than assuming support and finding out via a silently
// unverifiable run.
func (baseProvider) SupportsOutputSchema() bool { return false }

// EnforcesOutputSchema defaults to false for the same reason
// SupportsOutputSchema does — only a provider that actually forwards the
// schema flag to its CLI overrides this to true.
func (baseProvider) EnforcesOutputSchema() bool { return false }

func (baseProvider) OutputSchemaAsFile() bool { return false }

func (baseProvider) SessionFilePath(string) string { return "" }

var agentProviders = map[string]Provider{}

func registerAgentProvider(p Provider) {
	agentProviders[p.Name()] = p
}

func lookupProvider(name string) (Provider, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = providerid.Claude
	}
	if p, ok := agentProviders[key]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("unknown agent provider %q", name)
}

func providerByName(name string) Provider {
	p, err := lookupProvider(name)
	if err != nil {
		panic(err)
	}
	return p
}

func normalizeProvider(name string) string {
	return providerByName(name).Name()
}

func normalizeProviderName(name string) (string, error) {
	p, err := lookupProvider(name)
	if err != nil {
		return "", err
	}
	return p.Name(), nil
}

func normalizeModel(prov, model string) string {
	return providerByName(prov).NormalizeModel(model)
}

func providerForInvocation(a *Agent, cfg RunConfig) (Provider, error) {
	if cfg.provider != nil {
		return cfg.provider, nil
	}
	if a != nil && a.Provider != "" {
		return lookupProvider(a.Provider)
	}
	return lookupProvider(cfg.Provider)
}
