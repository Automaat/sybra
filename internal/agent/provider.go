package agent

import (
	"fmt"
	"strings"
	"time"

	providerpkg "github.com/Automaat/sybra/internal/provider"
)

type headlessInvocation struct {
	name    string
	args    []string
	env     []string
	command string
}

type perTurnConvoInvocation struct {
	bin  string
	args []string
}

type Provider interface {
	Name() string
	NormalizeModel(model string) string
	BuildCommand(cfg RunConfig, model string) string
	BuildHeadlessInvocation(a *Agent, cfg RunConfig) (headlessInvocation, error)
	ParseHeadlessLine(line []byte) (StreamEvent, error)
	SandboxArgs(requirePerms, headless bool) []string
	SupportsOutputSchema() bool
	UsesPerTurnConvo() bool
	BuildPerTurnConvoInvocation(a *Agent, cfg RunConfig, prompt string) perTurnConvoInvocation
	ParseConvoLine(line []byte) (ConvoEvent, error)
	SessionFilePath(sessionID string) string
	ClassifyError(sample providerpkg.ErrorSample) (providerpkg.Signal, string, time.Duration)
}

type baseProvider struct{}

func (baseProvider) SandboxArgs(bool, bool) []string { return nil }

func (baseProvider) SupportsOutputSchema() bool { return false }

func (baseProvider) UsesPerTurnConvo() bool { return false }

func (baseProvider) BuildPerTurnConvoInvocation(a *Agent, _ RunConfig, _ string) perTurnConvoInvocation {
	bin := strings.ToLower(strings.TrimSpace(a.Provider))
	if bin == "" {
		bin = "claude"
	}
	return perTurnConvoInvocation{bin: bin}
}

func (baseProvider) ParseConvoLine([]byte) (ConvoEvent, error) {
	return ConvoEvent{}, fmt.Errorf("provider does not support per-turn conversational parsing")
}

func (baseProvider) SessionFilePath(string) string { return "" }

var agentProviders = map[string]Provider{}

func registerAgentProvider(p Provider) {
	agentProviders[p.Name()] = p
}

func lookupProvider(name string) (Provider, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "claude"
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
